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
// command: re-read the provider checksums of every object recorded on a
// content-addressed destination and compare them against the
// fingerprints captured at upload time. Matches stamp the object
// verified; objects uploaded before fingerprint capture (or whose
// capture failed) get their fingerprint recorded on the first pass. A
// mismatch or a missing object is potential offsite corruption or
// tampering: it is reported per object and fails the command — verify
// changes nothing at the destination, the recorded fingerprint included,
// so the evidence is preserved for the operator.
func newVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify [<destination>]",
		Short: "Re-check recorded offsite objects against their upload fingerprints",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			destName := ""
			if len(args) == 1 {
				destName = args[0]
			}
			return runVerify(cmd, destName)
		},
	}
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
// content-addressed), or every content-addressed destination in config.
func verifyTargetNames(cfg *config.Config, destName string) ([]string, error) {
	if destName != "" {
		d, ok := cfg.Destinations[destName]
		if !ok {
			return nil, fmt.Errorf("unknown destination %q (declare it in %s)", destName, cfg.Path)
		}
		if d.Layout != config.LayoutContentAddressed {
			return nil, fmt.Errorf("destination %q has layout %q — verify covers the recorded objects of content-addressed destinations", destName, d.Layout)
		}
		return []string{destName}, nil
	}
	var names []string
	for name, d := range cfg.Destinations {
		if d.Layout == config.LayoutContentAddressed {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no content-addressed destinations declared in %s", cfg.Path)
	}
	sort.Strings(names)
	return names, nil
}

// printVerifyReport renders one destination's pass: a loud stderr line
// per missing or mismatched object, then the summary counters.
func printVerifyReport(out, errOut io.Writer, rep sync.RemoteVerifyReport, runErr error) {
	for _, hash := range rep.Missing {
		fmt.Fprintf(errOut, "error: object %s on %q: recorded as uploaded but absent from the remote\n", hash, rep.Destination)
	}
	for _, m := range rep.Mismatched {
		if m.Actual == "" {
			fmt.Fprintf(errOut, "error: object %s on %q: recorded %s %s, but the remote no longer exposes a %s checksum\n",
				m.Hash, rep.Destination, m.Algo, m.Recorded, m.Algo)
			continue
		}
		fmt.Fprintf(errOut, "error: object %s on %q: recorded %s %s, remote now reports %s — possible corruption or tampering\n",
			m.Hash, rep.Destination, m.Algo, m.Recorded, m.Actual)
	}
	if rep.Objects == 0 {
		fmt.Fprintf(out, "verify %s: no recorded objects\n", rep.Destination)
	} else {
		fmt.Fprintf(out, "verify %s: run=%d objects=%d verified=%d fingerprinted=%d pending=%d mismatched=%d missing=%d unrecorded=%d\n",
			rep.Destination, rep.RunID, rep.Objects, rep.Verified, rep.Populated, rep.Pending,
			len(rep.Mismatched), len(rep.Missing), rep.Unrecorded)
	}
	if runErr != nil {
		fmt.Fprintf(errOut, "verify %s: %v\n", rep.Destination, runErr)
	}
}
