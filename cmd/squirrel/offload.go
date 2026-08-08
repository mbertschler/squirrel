package main

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/offload"
	"github.com/mbertschler/squirrel/sync"
)

// newOffloadCmd returns the `squirrel offload <volume> [path...]` cobra
// command: delete the local bytes of files whose content is provably
// durable on every target the volume's offload_requires policy names.
// Paths are volume-relative files or directory prefixes; --older-than
// narrows by indexed mtime and combines with paths. At least one of the
// two selectors is required, and a volume without an offload_requires
// policy is refused outright.
func newOffloadCmd() *cobra.Command {
	var (
		olderThan string
		dryRun    bool
		perFile   bool
	)
	cmd := &cobra.Command{
		Use:   "offload <volume> [path...]",
		Short: "Delete local bytes whose content is durable on every required target",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOffload(cmd, args[0], args[1:], olderThan, dryRun, perFile)
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "", "only files whose indexed mtime is older than this duration (Go durations like 720h, or whole days like 90d)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report the durability gate decisions without deleting anything")
	cmd.Flags().BoolVar(&perFile, "per-file", false, "also list every blocked file with its own gate reasons (refusals are aggregated by target and cause otherwise)")
	return cmd
}

func runOffload(cmd *cobra.Command, volumeName string, paths []string, olderThanStr string, dryRun, perFile bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	vol, ok := cfg.Volumes[volumeName]
	if !ok {
		return fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
	}
	if len(vol.OffloadRequires) == 0 {
		return fmt.Errorf("volume %q has no offload_requires policy in %s; offload refuses to delete without an explicit list of required targets", volumeName, cfg.Path)
	}
	olderThan, err := parseOlderThan(olderThanStr)
	if err != nil {
		return err
	}

	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	relayedCaps, warns := gatherRelayedOffloadCaps(cmd.Context(), cfg, vol)
	for _, w := range warns {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}

	rep, err := offload.Offload(cmd.Context(), s, vol.Path, offload.Options{
		Name:           volumeName,
		Paths:          paths,
		OlderThan:      olderThan,
		Require:        vol.OffloadRequires,
		RequireDests:   requiredDestinations(cfg, vol.OffloadRequires),
		RelayedCaps:    relayedCaps,
		MaxEvidenceAge: vol.OffloadMaxEvidenceAge,
		VerifyCadenced: cfg.VerifyCadencedTargets(vol.OffloadRequires),
		DryRun:         dryRun,
	})
	printOffloadReport(cmd.OutOrStdout(), cmd.ErrOrStderr(), rep, dryRun, perFile)
	if err != nil {
		return err
	}
	if rep.Errors > 0 {
		return fmt.Errorf("%d file(s) failed during offload", rep.Errors)
	}
	return nil
}

// requiredDestinations resolves the volume's offload_requires target names
// to their local destination configs, for the offload capability
// pre-check. Names with no local destination — peer-relayed targets this
// node cannot see — are omitted; their capability is instead gathered from
// the owning peer (gatherRelayedOffloadCaps), falling back to the per-file
// gate when no peer can be reached.
func requiredDestinations(cfg *config.Config, require []string) map[string]*config.Destination {
	out := make(map[string]*config.Destination, len(require))
	for _, name := range require {
		if d, ok := cfg.Destinations[name]; ok {
			out[name] = d
		}
	}
	return out
}

// gatherRelayedOffloadCaps does the best-effort peer half of the offload
// capability pre-check (#145): for each peer-relayed required target (an
// offload_requires name that is neither a local destination nor a local
// node), it asks the peers this volume syncs to whether they can ever gate
// it, so a relayed target that can never gate aborts up front instead of
// sitting not-durable per file forever. Returns the gathered verdicts plus
// human-readable advisories for any peer that could not be reached — those
// targets simply fall back to the per-file gate, never a hard block.
func gatherRelayedOffloadCaps(ctx context.Context, cfg *config.Config, vol *config.Volume) ([]offload.RelayedTargetCapability, []string) {
	targets := peerRelayedTargets(cfg, vol)
	if len(targets) == 0 {
		return nil, nil
	}
	nodes := syncToNodes(cfg, vol)
	if len(nodes) == 0 {
		return nil, nil
	}
	caps, softErrs := sync.GatherRelayedCapabilities(ctx, vol.Name, nodes, targets)
	out := make([]offload.RelayedTargetCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, offload.RelayedTargetCapability{
			Target:  c.Target,
			Peer:    c.Peer,
			CanGate: c.CanGate,
			Reason:  c.Reason,
		})
	}
	warns := make([]string, 0, len(softErrs))
	for _, e := range softErrs {
		warns = append(warns, "offload capability pre-check skipped for "+e+" (falling back to the per-file gate)")
	}
	return out, warns
}

// peerRelayedTargets returns the offload_requires names that resolve to
// neither a local destination nor a local node: the peer-relayed targets
// whose gating capability this node can only learn from the peer that owns
// them. A local node target is excluded — this node pushes to it directly
// and records content-verified (peer-blake3) evidence, so it is always
// capable and needs no probe.
func peerRelayedTargets(cfg *config.Config, vol *config.Volume) map[string]struct{} {
	out := make(map[string]struct{})
	for _, name := range vol.OffloadRequires {
		if _, isDest := cfg.Destinations[name]; isDest {
			continue
		}
		if _, isNode := cfg.Nodes[name]; isNode {
			continue
		}
		out[name] = struct{}{}
	}
	return out
}

// syncToNodes returns the peer nodes this volume syncs to, in config order:
// the peers whose relayed durability (and now capability) this node pulls.
// A sync_to entry naming a destination rather than a node is skipped.
func syncToNodes(cfg *config.Config, vol *config.Volume) []*config.Node {
	var out []*config.Node
	for _, name := range vol.SyncTo {
		if node, ok := cfg.Nodes[name]; ok {
			out = append(out, node)
		}
	}
	return out
}

// olderThanDaysRE accepts the whole-day shorthand (e.g. "90d") that
// time.ParseDuration lacks.
var olderThanDaysRE = regexp.MustCompile(`^(\d+)d$`)

// parseOlderThan parses the --older-than flag: empty means no age
// selector, "<n>d" means n whole days, anything else must parse as a
// positive Go duration.
func parseOlderThan(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	var d time.Duration
	if m := olderThanDaysRE.FindStringSubmatch(s); m != nil {
		days, err := strconv.Atoi(m[1])
		if err != nil {
			return 0, fmt.Errorf("--older-than %q: %w", s, err)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		d, err = time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("--older-than %q: %w (use a Go duration like 720h, or whole days like 90d)", s, err)
		}
	}
	if d <= 0 {
		return 0, fmt.Errorf("--older-than must be a positive duration, got %s", s)
	}
	return d, nil
}
