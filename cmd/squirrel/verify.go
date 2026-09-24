package main

import (
	"fmt"
	"io"
	"slices"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/sync"
)

// newVerifyCmd returns the `squirrel verify [<destination>]` cobra
// command: re-read the provider checksums of every object and pack recorded
// on a content-addressed or packed destination and compare them against the
// fingerprints captured at upload time. Matches stamp the object/pack
// verified; artifacts uploaded before fingerprint capture (or whose capture
// failed) get their fingerprint recorded on the first pass. A mismatch or a
// missing object/pack is potential offsite corruption or tampering: it is
// reported per artifact and fails the command, with the destination and the
// recorded fingerprint left exactly as found so the operator inspects the
// evidence.
func newVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify [<destination>]",
		Short: "Re-check recorded offsite objects and packs against their upload fingerprints",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destName := ""
			if len(args) == 1 {
				destName = args[0]
			}
			return runVerify(cmd, destName)
		},
	}
	cmd.AddCommand(newVerifyAckCmd())
	return cmd
}

func runVerify(cmd *cobra.Command, destName string) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	names, err := verifyTargetNames(cfg, destName)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	out := cmd.OutOrStdout()
	rcl, err := verifyRclone(cmd, cfg, names)
	if err != nil {
		return err
	}

	var anyFailed bool
	for _, name := range names {
		rep, err := sync.VerifyRemote(cmd.Context(), s, rcl, cfg.Destinations[name])
		printVerifyReport(out, cmd.ErrOrStderr(), rep, err)
		if err != nil || !rep.Clean() {
			anyFailed = true
		}
	}
	if anyFailed {
		return fmt.Errorf("one or more destinations failed verification")
	}
	return nil
}

// verifyRclone locates rclone and renders its config when a target is
// reached through it; a native destination is verified without it.
func verifyRclone(cmd *cobra.Command, cfg *config.Config, names []string) (*sync.Rclone, error) {
	if !slices.ContainsFunc(names, func(name string) bool {
		return sync.Pair{Destination: cfg.Destinations[name]}.DrivesRclone()
	}) {
		return nil, nil
	}
	rcl, err := sync.Find(cmd.Context())
	if err != nil {
		return nil, err
	}
	if err := writeRcloneConfigLogged(cmd.OutOrStdout(), rcl, cfg); err != nil {
		return nil, err
	}
	return rcl, nil
}

// verifyTargetNames resolves the verification subjects in deterministic
// order: an explicit destination (validated to exist and be verifiable),
// or every verifiable destination in config.
func verifyTargetNames(cfg *config.Config, destName string) ([]string, error) {
	if destName != "" {
		d, ok := cfg.Destinations[destName]
		if !ok {
			return nil, fmt.Errorf("unknown destination %q (declare it in %s)", destName, cfg.Path)
		}
		if !d.Verifiable() {
			return nil, fmt.Errorf("destination %q is an rclone mirror — verify covers the recorded objects and packs of content-addressed and packed destinations, and the recorded copies of a mirror squirrel writes itself (local, or sftp without crypt)", destName)
		}
		return []string{destName}, nil
	}
	var names []string
	for name, d := range cfg.Destinations {
		if d.Verifiable() {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no verifiable destinations declared in %s: verify covers content-addressed and packed destinations and native mirrors", cfg.Path)
	}
	sort.Strings(names)
	return names, nil
}

// printVerifyReport renders one destination's pass: a loud stderr line
// per missing or mismatched object or pack, then the summary counters,
// then the standing-alarm transition this pass caused (#157, F30).
func printVerifyReport(out, errOut io.Writer, rep sync.RemoteVerifyReport, runErr error) {
	printVerifyFailures(errOut, "object", rep.Destination, rep.Missing, rep.Mismatched)
	printVerifyFailures(errOut, "pack", rep.Destination, rep.PacksMissing, rep.PackMismatched)
	printMirrorFailures(errOut, rep)
	if runErr != nil {
		fmt.Fprintf(errOut, "verify %s: %v\n", rep.Destination, runErr)
		return
	}
	if rep.Paths > 0 {
		fmt.Fprintf(out, "verify %s: run=%d copies=%d reread=%d changed=%d missing=%d\n",
			rep.Destination, rep.RunID, rep.Paths, rep.PathsReread, len(rep.PathsChanged), len(rep.PathsMissing))
		printVerifyAlarmTransition(out, errOut, rep)
		return
	}
	if rep.Objects == 0 && rep.Packs == 0 {
		fmt.Fprintf(out, "verify %s: nothing recorded to verify\n", rep.Destination)
		return
	}
	fmt.Fprintf(out, "verify %s: run=%d objects=%d verified=%d fingerprinted=%d pending=%d mismatched=%d missing=%d unrecorded=%d packs=%d packs_verified=%d packs_fingerprinted=%d packs_pending=%d packs_mismatched=%d packs_missing=%d\n",
		rep.Destination, rep.RunID, rep.Objects, rep.Verified, rep.Populated, rep.Pending,
		len(rep.Mismatched), len(rep.Missing), rep.Unrecorded,
		rep.Packs, rep.PacksVerified, rep.PacksPopulated, rep.PacksPending,
		len(rep.PackMismatched), len(rep.PacksMissing))
	printVerifyAlarmTransition(out, errOut, rep)
}

// printVerifyAlarmTransition surfaces the standing-alarm change this pass
// made: a raised alarm shouts on stderr with how to clear it, an
// auto-cleared alarm is a quiet confidence line on stdout.
func printVerifyAlarmTransition(out, errOut io.Writer, rep sync.RemoteVerifyReport) {
	if rep.AlarmRaised {
		fmt.Fprintf(errOut, "ALARM %s: verification failed — this destination stays in alarm until a clean `squirrel verify %s` clears it or you run `squirrel verify ack %s`\n",
			rep.Destination, rep.Destination, rep.Destination)
	}
	if rep.AlarmCleared {
		fmt.Fprintf(out, "verify %s: standing alarm cleared by this clean pass\n", rep.Destination)
	}
}

// printMirrorFailures writes one stderr line per mirror copy the pass found
// gone or changed; the next push writes each one again that the index still
// holds.
func printMirrorFailures(errOut io.Writer, rep sync.RemoteVerifyReport) {
	for _, name := range rep.PathsMissing {
		fmt.Fprintf(errOut, "error: copy %s on %q: recorded as stored but absent\n", name, rep.Destination)
	}
	for _, name := range rep.PathsChanged {
		fmt.Fprintf(errOut, "error: copy %s on %q: its size, mtime or BLAKE3 no longer matches what squirrel stored — possible corruption or tampering\n", name, rep.Destination)
	}
}

// printVerifyFailures writes one stderr line per missing or mismatched
// artifact (an object or a pack), naming the kind so a mixed destination's
// failures stay distinguishable.
func printVerifyFailures(errOut io.Writer, kind, dest string, missing []string, mismatched []sync.RemoteObjectMismatch) {
	for _, hash := range missing {
		fmt.Fprintf(errOut, "error: %s %s on %q: recorded as uploaded but absent from the remote\n", kind, hash, dest)
	}
	for _, m := range mismatched {
		if m.Actual == "" {
			fmt.Fprintf(errOut, "error: %s %s on %q: recorded %s %s, but the remote no longer exposes a %s checksum\n",
				kind, m.Hash, dest, m.Algo, m.Recorded, m.Algo)
			continue
		}
		fmt.Fprintf(errOut, "error: %s %s on %q: recorded %s %s, remote now reports %s — possible corruption or tampering\n",
			kind, m.Hash, dest, m.Algo, m.Recorded, m.Actual)
	}
}
