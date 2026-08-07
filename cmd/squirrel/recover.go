package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/status"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// newRecoverCmd builds `squirrel recover` — the guided form of the disaster
// runbook (#194, F31). Every mechanism it drives already worked on its own;
// what was missing was the connective tissue, and assembling the sequence
// took tool-author knowledge at the moment the operator has least of it.
//
// It sequences and confirms. It does not decide: nothing here runs
// unattended, nothing is chosen for the operator that they cannot see
// first, and the default is to say what would happen and stop.
func newRecoverCmd() *cobra.Command {
	var (
		from     string
		snapshot string
		execute  bool
		assumeOK bool
		force    bool
	)
	cmd := &cobra.Command{
		Use:   "recover",
		Short: "Walk through recovering this machine from a destination that holds its index and bytes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRecover(cmd, recoverOptions{
				From:     from,
				Snapshot: snapshot,
				Execute:  execute,
				AssumeOK: assumeOK,
				Force:    force,
			})
		},
	}
	cmd.Flags().StringVar(&from, "from", "", "destination name to recover from (required)")
	cmd.Flags().StringVar(&snapshot, "snapshot", "", "index snapshot filename to install (default: the newest found)")
	cmd.Flags().BoolVar(&execute, "execute", false, "carry out the plan instead of only describing it")
	cmd.Flags().BoolVar(&assumeOK, "yes", false, "answer every phase confirmation with yes (for a rehearsed, unattended recovery)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the running-agent check when installing the index")
	return cmd
}

// recoverOptions is the resolved flag set for one invocation.
type recoverOptions struct {
	From     string
	Snapshot string
	Execute  bool
	AssumeOK bool
	Force    bool
}

// recoverPlan is what discovery found and what it implies — assembled
// before anything is touched, so the operator reads the whole sequence
// before agreeing to its first step.
type recoverPlan struct {
	dest      *config.Destination
	volumes   []string
	snapshots []sync.IndexSnapshot
	chosen    sync.IndexSnapshot
	peers     []string
}

// runRecover drives the three phases in the order the runbook proves is the
// only safe one: index first, then bytes, then peers. The ordering is not a
// preference — restoring volumes before the catalog means restoring with
// nothing to verify against, and re-pairing peers before the index is back
// means a peer syncing into an empty node.
func runRecover(cmd *cobra.Command, opts recoverOptions) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	if opts.From == "" {
		return fmt.Errorf("--from is required: name the destination that holds this machine's index snapshots (one of %s)",
			strings.Join(destinationNames(cfg), ", "))
	}
	plan, err := discoverRecovery(cmd, cfg, opts)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	printRecoveryPlan(out, plan, opts)
	if !opts.Execute {
		fmt.Fprintf(out, "\nNothing has been touched. Re-run with --execute to begin, "+
			"confirming each phase as it comes.\n")
		return nil
	}
	return executeRecovery(cmd, cfg, plan, opts)
}

// discoverRecovery answers "what is recoverable, and from where" without
// touching anything. It is the phase the issue asks for first and the one
// an operator may legitimately run alone, repeatedly, while deciding.
func discoverRecovery(cmd *cobra.Command, cfg *config.Config, opts recoverOptions) (recoverPlan, error) {
	dest, ok := cfg.Destinations[opts.From]
	if !ok {
		if _, isNode := cfg.Nodes[opts.From]; isNode {
			return recoverPlan{}, fmt.Errorf("%q is a peer node, not a destination: recovering a dead edge machine "+
				"is a reverse peer push from the surviving hub, not a recover run — see the recovery guide", opts.From)
		}
		return recoverPlan{}, fmt.Errorf("unknown destination %q (declared: %s)",
			opts.From, strings.Join(destinationNames(cfg), ", "))
	}
	plan := recoverPlan{dest: dest, volumes: volumesSyncingTo(cfg, opts.From), peers: nodeNames(cfg)}
	if len(plan.volumes) == 0 {
		return recoverPlan{}, fmt.Errorf("no volume in %s declares sync_to = [… %q …], so this destination holds nothing for this machine",
			cfg.Path, opts.From)
	}

	rcl, err := sync.Find()
	if err != nil {
		return recoverPlan{}, err
	}
	if err := writeRcloneConfigLogged(cmd.OutOrStdout(), rcl, cfg); err != nil {
		return recoverPlan{}, err
	}
	plan.snapshots, err = sync.DiscoverIndexSnapshots(cmd.Context(), rcl, dest, plan.volumes)
	if err != nil {
		return recoverPlan{}, err
	}
	plan.chosen, err = chooseSnapshot(plan.snapshots, opts.Snapshot, dest.Name)
	if err != nil {
		return recoverPlan{}, err
	}
	return plan, nil
}

// chooseSnapshot picks the snapshot to install: the operator's if they
// named one, otherwise the newest. An empty listing is a hard stop with the
// reason spelled out, because "no snapshots" during a recovery is the one
// answer an operator must not have to infer from a blank section.
func chooseSnapshot(snaps []sync.IndexSnapshot, want, destName string) (sync.IndexSnapshot, error) {
	if len(snaps) == 0 {
		return sync.IndexSnapshot{}, fmt.Errorf("no index snapshots found under %s — this destination carries bytes but no catalog, "+
			"so the volumes can still be restored by hand with `squirrel restore` once an index exists, "+
			"but there is nothing here to recover the index from", destName)
	}
	if want == "" {
		return snaps[0], nil
	}
	for _, s := range snaps {
		if s.Name == want {
			return s, nil
		}
	}
	return sync.IndexSnapshot{}, fmt.Errorf("snapshot %q not found on %s; run without --snapshot to list what is there", want, destName)
}

// printRecoveryPlan states what was found and what each phase will do
// before any of it happens.
func printRecoveryPlan(out io.Writer, plan recoverPlan, opts recoverOptions) {
	now := time.Now()
	fmt.Fprintf(out, "recovering from destination %s (%s)\n\n", plan.dest.Name, plan.dest.Type)

	fmt.Fprintf(out, "index snapshots found (%d):\n", len(plan.snapshots))
	for _, s := range plan.snapshots {
		marker := "  "
		if s.Name == plan.chosen.Name && s.Volume == plan.chosen.Volume {
			marker = "> "
		}
		fmt.Fprintf(out, "%s%s  %s  %s\n", marker, s.Volume, s.Name, snapshotAge(s, now))
	}

	fmt.Fprintf(out, "\nphase 1 — install the index\n")
	fmt.Fprintf(out, "  fetch %s (%s) and make it the live index.\n", plan.chosen.Name, snapshotAge(plan.chosen, now))
	fmt.Fprintf(out, "  any existing index is preserved beside it, not deleted.\n")

	fmt.Fprintf(out, "\nphase 2 — restore the volumes (%d)\n", len(plan.volumes))
	for _, v := range plan.volumes {
		fmt.Fprintf(out, "  %s ← %s\n", v, plan.dest.Name)
	}
	fmt.Fprintf(out, "  each is a normal `squirrel restore`, verified against the index from phase 1.\n")

	if len(plan.peers) > 0 {
		fmt.Fprintf(out, "\nphase 3 — re-pair the peers (%d)\n", len(plan.peers))
		for _, p := range plan.peers {
			fmt.Fprintf(out, "  %s\n", p)
		}
		fmt.Fprintf(out, "  squirrel prints the commands; pairing needs both machines, so it does not run them.\n")
	}
	if opts.Snapshot == "" && len(plan.snapshots) > 1 {
		fmt.Fprintf(out, "\n(--snapshot <name> installs a different one; the newest is the default.)\n")
	}
}

// snapshotAge renders a snapshot's age, or says plainly that the filename
// does not carry one rather than implying freshness it cannot vouch for.
func snapshotAge(s sync.IndexSnapshot, now time.Time) string {
	age, ok := s.Age(now)
	if !ok {
		return "(age unknown — filename not in the expected form)"
	}
	return status.HumanAge(age) + " old"
}

// executeRecovery runs the phases, each behind its own confirmation, and
// stops at the first refusal or failure rather than half-recovering.
func executeRecovery(cmd *cobra.Command, cfg *config.Config, plan recoverPlan, opts recoverOptions) error {
	out := cmd.OutOrStdout()
	if err := recoverIndexPhase(cmd, plan, opts); err != nil {
		return err
	}
	if err := recoverVolumesPhase(cmd, plan, opts); err != nil {
		return err
	}
	printRepairGuidance(out, cfg, plan)
	fmt.Fprintf(out, "\nrecovery complete. `squirrel status` now answers for this machine again.\n")
	return nil
}

// recoverIndexPhase fetches the chosen snapshot and installs it as the live
// index, then records the recovery in the database that has just become the
// live one.
func recoverIndexPhase(cmd *cobra.Command, plan recoverPlan, opts recoverOptions) error {
	out := cmd.OutOrStdout()
	if !confirmPhase(cmd, opts, fmt.Sprintf(
		"phase 1: fetch %s and make it this machine's index?", plan.chosen.Name)) {
		return nil
	}

	rcl, err := sync.Find()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "squirrel-recover-")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	staged := filepath.Join(dir, plan.chosen.Name)

	fmt.Fprintf(out, "fetching %s …\n", plan.chosen.Name)
	if err := sync.FetchIndexSnapshot(cmd.Context(), rcl, plan.dest, plan.chosen, staged); err != nil {
		return err
	}
	// runDBRestore preflights the schema version, refuses to clobber a live
	// DB another process holds open, and preserves the outgoing one. Calling
	// it rather than reimplementing is the point of the verb: the guided
	// flow sequences the mechanisms, it does not fork them.
	if err := runDBRestore(cmd, staged, opts.Force); err != nil {
		return fmt.Errorf("install recovered index: %w", err)
	}
	recordRecovery(cmd, plan)
	return nil
}

// recordRecovery writes the audit run for the install. A failure here is
// reported but never fails the phase: the index is already in place, and
// refusing to continue a disaster recovery over a missing bookkeeping row
// would be the wrong trade.
func recordRecovery(cmd *cobra.Command, plan recoverPlan) {
	out := cmd.OutOrStdout()
	cfg, _ := tryLoadConfig(cmd)
	s, err := openStore(cmd, cfg)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: recovered index installed but could not be opened to record the recovery: %v\n", err)
		return
	}
	defer s.Close()

	ctx := cmd.Context()
	runID, err := s.BeginRecoveryRun(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not record the recovery run: %v\n", err)
		return
	}
	note := fmt.Sprintf("snapshot=%s volume=%s destination=%s", plan.chosen.Name, plan.chosen.Volume, plan.dest.Name)
	if err := s.AppendRunAudit(ctx, store.RunAuditEntry{
		RunID: runID, Transition: store.TransitionRecoverIndex, Note: note,
	}); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not record the recovery note: %v\n", err)
	}
	if err := s.FinishRun(ctx, runID, store.RunStatusSuccess, "", 0); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: could not finish the recovery run: %v\n", err)
		return
	}
	fmt.Fprintf(out, "recorded recovery as run %d\n", runID)
}

// recoverVolumesPhase restores each volume the destination carries, one
// confirmation for the phase rather than one per volume — the operator has
// already seen the list, and a per-volume prompt on a five-volume hub is
// ceremony, not consent.
func recoverVolumesPhase(cmd *cobra.Command, plan recoverPlan, opts recoverOptions) error {
	out := cmd.OutOrStdout()
	if !confirmPhase(cmd, opts, fmt.Sprintf(
		"phase 2: restore %d volume(s) from %s into their configured paths?", len(plan.volumes), plan.dest.Name)) {
		return nil
	}
	// The config was read before the index was replaced; volumes are keyed
	// by name in both, so the list stays valid across the install.
	for _, name := range plan.volumes {
		fmt.Fprintf(out, "\nrestoring %s …\n", name)
		if err := runRestore(cmd, name, plan.dest.Name, sync.RestoreOptions{}); err != nil {
			return fmt.Errorf("restore %s: %w — recovery stopped here; the index is installed and "+
				"the volumes already restored are intact, so re-running continues from this volume", name, err)
		}
	}
	return nil
}

// printRepairGuidance names the commands that re-establish each peer
// relationship. It prints rather than runs: pairing writes to both
// machines' configs and needs the peer reachable and consenting, which is
// not something a recovery on this side can assume.
func printRepairGuidance(out io.Writer, cfg *config.Config, plan recoverPlan) {
	if len(plan.peers) == 0 {
		return
	}
	fmt.Fprintf(out, "\nphase 3 — re-pair the peers\n")
	fmt.Fprintf(out, "  this machine's index and bytes are back; its peer trust material is not.\n")
	fmt.Fprintf(out, "  if this machine's TLS identity was lost with it, mint a new one first:\n")
	fmt.Fprintf(out, "      squirrel agent cert\n")
	fmt.Fprintf(out, "  then, for each peer, re-pair from whichever side is reachable:\n")
	for _, p := range plan.peers {
		node := cfg.Nodes[p]
		fmt.Fprintf(out, "      squirrel node pair %s    # %s\n", p, node.Endpoint)
	}
	fmt.Fprintf(out, "  `squirrel config check` then confirms every pairing before the first sync.\n")
}

// confirmPhase asks once, on stderr-adjacent stdout, and treats anything
// but an explicit yes as a stop. --yes answers for a rehearsed recovery;
// a non-interactive stdin without --yes is a refusal, not an implied yes.
func confirmPhase(cmd *cobra.Command, opts recoverOptions, question string) bool {
	out := cmd.OutOrStdout()
	if opts.AssumeOK {
		fmt.Fprintf(out, "\n%s yes (--yes)\n", question)
		return true
	}
	fmt.Fprintf(out, "\n%s [y/N] ", question)
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintf(out, "\nno answer read; stopping without changing anything further.\n")
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	fmt.Fprintf(out, "stopped. Nothing further has been changed.\n")
	return false
}

// volumesSyncingTo returns the config's volumes that push to the named
// target, in name order — the volumes that destination can hold.
func volumesSyncingTo(cfg *config.Config, target string) []string {
	var out []string
	for _, name := range sortedKeys(cfg.Volumes) {
		if slices.Contains(cfg.Volumes[name].SyncTo, target) {
			out = append(out, name)
		}
	}
	return out
}

// destinationNames and nodeNames list the config's declared names for error
// messages and the re-pairing phase.
func destinationNames(cfg *config.Config) []string { return sortedKeys(cfg.Destinations) }
func nodeNames(cfg *config.Config) []string        { return sortedKeys(cfg.Nodes) }
