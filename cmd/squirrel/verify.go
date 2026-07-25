package main

import (
	"fmt"
	"io"
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

	rcl, err := sync.Find()
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if err := writeRcloneConfigLogged(out, rcl, cfg); err != nil {
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

// verifyTargetNames resolves the verification subjects in deterministic
// order: an explicit destination (validated to exist and be
// content-addressed or packed), or every such destination in config.
func verifyTargetNames(cfg *config.Config, destName string) ([]string, error) {
	if destName != "" {
		d, ok := cfg.Destinations[destName]
		if !ok {
			return nil, fmt.Errorf("unknown destination %q (declare it in %s)", destName, cfg.Path)
		}
		if !verifiableLayout(d.Layout) {
			return nil, fmt.Errorf("destination %q has layout %q — verify covers the recorded objects and packs of content-addressed and packed destinations", destName, d.Layout)
		}
		return []string{destName}, nil
	}
	var names []string
	for name, d := range cfg.Destinations {
		if verifiableLayout(d.Layout) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no content-addressed or packed destinations declared in %s", cfg.Path)
	}
	sort.Strings(names)
	return names, nil
}

// verifiableLayout reports whether a destination's layout keeps per-object
// or per-pack fingerprints that `squirrel verify` re-checks.
func verifiableLayout(layout string) bool {
	return layout == config.LayoutContentAddressed || layout == config.LayoutPacked
}

// printVerifyReport renders one destination's pass: a loud stderr line
// per missing or mismatched object or pack, then the summary counters,
// then the standing-alarm transition this pass caused (#157, F30).
func printVerifyReport(out, errOut io.Writer, rep sync.RemoteVerifyReport, runErr error) {
	printVerifyFailures(errOut, "object", rep.Destination, rep.Missing, rep.Mismatched)
	printVerifyFailures(errOut, "pack", rep.Destination, rep.PacksMissing, rep.PackMismatched)
	if runErr != nil {
		fmt.Fprintf(errOut, "verify %s: %v\n", rep.Destination, runErr)
		return
	}
	if rep.Objects == 0 && rep.Packs == 0 {
		fmt.Fprintf(out, "verify %s: no recorded objects or packs\n", rep.Destination)
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
