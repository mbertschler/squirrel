package main

import (
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/sync"
)

// newRunsCmd returns the `squirrel runs` cobra command. It lists rows from
// the runs table, most-recent first, with optional volume filtering and a
// configurable cap. Runs of every kind (index, sync, audit, restore,
// offload) and every status (running, success, failed, partial, refused,
// aborted) are surfaced so in-flight, failed, or refused work shows up
// alongside successful runs. --failed and --changes narrow steady-state
// noise (friction F19).
func newRunsCmd() *cobra.Command {
	var (
		volumeName  string
		limit       int
		onlyFailed  bool
		onlyChanges bool
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List runs of every kind (most recent first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuns(cmd, volumeName, limit, onlyFailed, onlyChanges)
		},
	}
	cmd.Flags().StringVar(&volumeName, "volume", "", "filter to runs against this volume name")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of runs to show (0 for no limit)")
	cmd.Flags().BoolVar(&onlyFailed, "failed", false, "show only runs that need attention (failed, refused, aborted, or partial)")
	cmd.Flags().BoolVar(&onlyChanges, "changes", false, "hide clean no-op runs; show only runs that moved content or need attention")
	cmd.AddCommand(newRunsFailCmd())
	return cmd
}

func runRuns(cmd *cobra.Command, volumeName string, limit int, onlyFailed, onlyChanges bool) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	volumeID, err := resolveVolumeFilter(cmd, s, volumeName)
	if err != nil {
		return err
	}
	runs, conflicts, err := gatherRuns(cmd, s, volumeID, limit, onlyFailed, onlyChanges)
	if err != nil {
		return err
	}
	volumes, err := loadVolumeNames(cmd, s)
	if err != nil {
		return err
	}
	nodes, err := loadNodeNames(cmd, s, runs)
	if err != nil {
		return err
	}
	if err := printRuns(cmd.OutOrStdout(), runs, volumes, nodes, conflicts); err != nil {
		return err
	}
	alarms, err := s.ListDestinationAlarms(cmd.Context())
	if err != nil {
		return fmt.Errorf("list destination alarms: %w", err)
	}
	printActiveAlarms(cmd.OutOrStdout(), alarms)
	contested, err := s.ListContestedPaths(cmd.Context())
	if err != nil {
		return fmt.Errorf("list contested paths: %w", err)
	}
	printContestedReminder(cmd.OutOrStdout(), contested, volumeID, volumes)
	return nil
}

// printContestedReminder surfaces standing contested freezes beneath the
// runs listing (#158, F27). Like the alarms reminder, a single old
// conflict run scrolls away, so the audit surface repeats the standing
// condition until a human resolves it — and points at the dedicated
// `squirrel conflicts` question-command for the detail. When the listing
// was scoped with --volume (filterVolumeID non-nil), the reminder is
// scoped to match rather than reporting freezes on unrelated volumes.
// Volumes render by name via the shared label map, never by internal id.
func printContestedReminder(w io.Writer, contested []store.ContestedPath, filterVolumeID *int64, volumes map[int64]string) {
	scoped := contested
	if filterVolumeID != nil {
		scoped = scoped[:0:0]
		for _, c := range contested {
			if c.VolumeID == *filterVolumeID {
				scoped = append(scoped, c)
			}
		}
	}
	if len(scoped) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d contested path(s) frozen — inspect with `squirrel conflicts`, clear with `squirrel conflicts resolve`:\n",
		len(scoped))
	for _, c := range scoped {
		fmt.Fprintf(w, "  CONTESTED volume=%s %s since %s\n",
			volumeLabel(sql.NullInt64{Int64: c.VolumeID, Valid: true}, volumes),
			c.Path, formatStarted(c.RaisedAtNs))
	}
}

// printActiveAlarms surfaces standing per-destination alarms beneath the
// runs listing (#157, F30). A verify mismatch latches an alarm that must
// stay visible until an operator clears it — a single old run row scrolls
// away, so the audit surface repeats the standing condition on every
// `squirrel runs` until it is resolved.
func printActiveAlarms(w io.Writer, alarms []store.DestinationAlarm) {
	if len(alarms) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%d destination(s) in alarm — clear with a clean `squirrel verify` or `squirrel verify ack <dest>`:\n",
		len(alarms))
	for _, a := range alarms {
		fmt.Fprintf(w, "  ALARM %s (%s) since %s, run %d: %s\n",
			a.Destination, a.Kind,
			formatStarted(a.RaisedAtNs), a.RaisedRunID, a.Detail)
	}
}

// resolveVolumeFilter resolves an optional --volume name to its id, shared by
// the runs query and the standing-condition reminders printed beneath the
// listing so both are scoped the same way. An empty name means every volume
// and yields nil.
func resolveVolumeFilter(cmd *cobra.Command, s *store.Store, volumeName string) (*int64, error) {
	if volumeName == "" {
		return nil, nil
	}
	v, err := s.GetVolumeByName(cmd.Context(), volumeName)
	if err != nil {
		if store.IsNotFound(err) {
			return nil, fmt.Errorf("no volume named %q", volumeName)
		}
		return nil, fmt.Errorf("lookup volume %q: %w", volumeName, err)
	}
	return &v.ID, nil
}

// gatherRuns fetches runs under an already-resolved volume filter, applies the
// --failed/--changes predicates, and returns the rows to display plus the
// conflict counts for their CONFLICTS column. When a predicate is active the
// fetch is unbounded (opts.Limit=0) so the SQL LIMIT can't cap before the
// predicate runs and silently drop matches; the result is truncated to limit
// after filtering instead.
//
// Conflict counts are loaded only where they are actually needed, never
// across the whole unbounded fetch: --failed needs none for filtering, and
// the CONFLICTS column only covers the displayed rows. This keeps the id set
// handed to the count query small (and the query itself batches ids), so a
// large peer-sync history can't overflow SQLite's bound-parameter cap.
func gatherRuns(cmd *cobra.Command, s *store.Store, volumeID *int64, limit int, onlyFailed, onlyChanges bool) ([]store.Run, map[int64]int, error) {
	filtering := onlyFailed || onlyChanges
	opts := store.ListRunsOpts{Limit: limit, Descending: true, VolumeID: volumeID}
	if filtering {
		opts.Limit = 0
	}
	runs, err := s.ListRuns(cmd.Context(), opts)
	if err != nil {
		return nil, nil, err
	}
	if filtering {
		filterConflicts, err := conflictCountsForFilter(cmd, s, runs, onlyFailed)
		if err != nil {
			return nil, nil, err
		}
		runs = filterRuns(runs, filterConflicts, onlyFailed, onlyChanges)
		if limit > 0 && len(runs) > limit {
			runs = runs[:limit]
		}
	}
	// Load the CONFLICTS-column counts for exactly the displayed rows.
	conflicts, err := loadConflictCounts(cmd, s, runs)
	if err != nil {
		return nil, nil, err
	}
	return runs, conflicts, nil
}

// conflictCountsForFilter loads only the conflict counts the active filter
// needs. --failed consults none — an attention-worthy run is unconditionally
// kept — so it returns an empty map. --changes needs them only for the rows
// that would otherwise be hidden as no-ops, so it counts just those rather
// than the whole unbounded fetch.
func conflictCountsForFilter(cmd *cobra.Command, s *store.Store, runs []store.Run, onlyFailed bool) (map[int64]int, error) {
	if onlyFailed {
		return map[int64]int{}, nil
	}
	var candidates []store.Run
	for _, r := range runs {
		if r.NoOp() {
			candidates = append(candidates, r)
		}
	}
	return loadConflictCounts(cmd, s, candidates)
}

// filterRuns applies the --failed / --changes predicates in one pass,
// preserving the descending order the store returned.
func filterRuns(runs []store.Run, conflicts map[int64]int, onlyFailed, onlyChanges bool) []store.Run {
	out := make([]store.Run, 0, len(runs))
	for _, r := range runs {
		if onlyFailed && !runNeedsAttention(r.Status) {
			continue
		}
		if onlyChanges && !runIsInteresting(r, conflicts) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// runNeedsAttention reports whether a run ended in a state an operator
// should look at: a mid-flight failure, a preflight refusal, a reaped
// (aborted) run, or a partial completion.
func runNeedsAttention(status string) bool {
	switch status {
	case store.RunStatusFailed, store.RunStatusRefused, store.RunStatusAborted, store.RunStatusPartial:
		return true
	}
	return false
}

// runIsInteresting reports whether a run is worth showing under --changes:
// anything needing attention, still running, that changed files, or that
// resolved conflicts. A clean success that changed nothing is the routine
// no-op the filter hides.
//
// store.Run.NoOp owns the "changed nothing" rule so this filter and the
// TUI's `f` fold hide exactly the same rows: the run's recorded
// changed_count where it has one, and the conservative file_count
// heuristic for the pre-v28 history and the drivers that report no count.
func runIsInteresting(r store.Run, conflicts map[int64]int) bool {
	if !r.NoOp() {
		return true
	}
	return conflicts[r.ID] > 0
}

// loadNodeNames builds an id→name map for the peer_node_id values
// referenced by the listing. Only peer-sync rows carry a non-NULL
// peer_node_id, so this typically resolves to a small set of distinct
// nodes; we fetch only those rather than every row in `nodes`.
func loadNodeNames(cmd *cobra.Command, s *store.Store, runs []store.Run) (map[int64]string, error) {
	out := map[int64]string{}
	seen := map[int64]struct{}{}
	for _, r := range runs {
		if !r.PeerNodeID.Valid {
			continue
		}
		id := r.PeerNodeID.Int64
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		node, err := s.GetNodeByID(cmd.Context(), id)
		if err != nil {
			if store.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("lookup node id=%d: %w", id, err)
		}
		out[id] = node.Name
	}
	return out, nil
}

// loadConflictCounts derives the conflict count for every peer-sync run in
// the listing. The schema doesn't carry the count on the run row itself,
// so it comes from two derivations merged per run:
//
//   - Receiver side: every conflict inserts one row under
//     `.squirrel-conflicts/run-<id>/`, so the prefix-count is the number
//     of losers this run preserved.
//   - Initiator side: the conflict was frozen on a *remote* receiver, so
//     no local `.squirrel-conflicts/` rows exist; instead the run raised
//     local contested_paths latches (one per conflict + contested
//     refusal), so the raised-by-run count is the initiator's signal
//     (#158, F27).
//
// A run is one side of a sync, so at most one derivation is non-zero for
// it; taking the max yields the right figure without double-counting the
// rare case where both happen to fire on the same id.
func loadConflictCounts(cmd *cobra.Command, s *store.Store, runs []store.Run) (map[int64]int, error) {
	var peerSyncIDs []int64
	for _, r := range runs {
		if r.Kind == store.RunKindSync && r.PeerNodeID.Valid {
			peerSyncIDs = append(peerSyncIDs, r.ID)
		}
	}
	if len(peerSyncIDs) == 0 {
		return map[int64]int{}, nil
	}
	preserved, err := s.CountFilesFirstSeenByRunWithPathPrefix(cmd.Context(), peerSyncIDs, sync.ConflictsDirName)
	if err != nil {
		return nil, fmt.Errorf("conflict counts: %w", err)
	}
	frozen, err := s.CountContestedRaisedByRun(cmd.Context(), peerSyncIDs)
	if err != nil {
		return nil, fmt.Errorf("contested counts: %w", err)
	}
	counts := make(map[int64]int, len(preserved)+len(frozen))
	for id, n := range preserved {
		counts[id] = n
	}
	for id, n := range frozen {
		if n > counts[id] {
			counts[id] = n
		}
	}
	return counts, nil
}

// loadVolumeNames builds an id→name map for rendering runs. Loading once up
// front avoids a per-run lookup; for typical volume counts the map is tiny.
func loadVolumeNames(cmd *cobra.Command, s *store.Store) (map[int64]string, error) {
	vols, err := s.ListVolumes(cmd.Context())
	if err != nil {
		return nil, fmt.Errorf("list volumes: %w", err)
	}
	out := make(map[int64]string, len(vols))
	for _, v := range vols {
		out[v.ID] = v.Name
	}
	return out, nil
}

func printRuns(out io.Writer, runs []store.Run, volumes map[int64]string, nodes map[int64]string, conflicts map[int64]int) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tVOLUME\tDESTINATION\tPEER\tSTARTED\tDURATION\tSTATUS\tFILES\tCONFLICTS\tERROR")
	for _, r := range runs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, r.Kind, volumeLabel(r.VolumeID, volumes),
			destinationLabel(r.Destination),
			peerLabel(r, nodes),
			formatStarted(r.StartedAtNs),
			formatDuration(r.StartedAtNs, r.EndedAtNs),
			r.Status, r.FileCount, conflictLabel(r, conflicts),
			truncateError(r.Error),
		)
	}
	return tw.Flush()
}

// peerLabel formats the peer attribution for a runs row. Bucket-sync
// and non-sync rows print "—"; peer-sync rows print
// "<peer-name> correlated=<id>" so the operator can pair the two
// halves of one logical sync against the receiver's `squirrel runs`
// without scrolling between separate columns. A dropped node row
// (FK should prevent this, but defensive) prints "id=N" rather than
// silently rendering nothing.
func peerLabel(r store.Run, nodes map[int64]string) string {
	if !r.PeerNodeID.Valid {
		return "—"
	}
	name, ok := nodes[r.PeerNodeID.Int64]
	if !ok {
		name = fmt.Sprintf("id=%d", r.PeerNodeID.Int64)
	}
	label := peerDirectionArrow(r) + " " + name
	if r.CorrelatedRunID.Valid {
		return fmt.Sprintf("%s correlated=%d", label, r.CorrelatedRunID.Int64)
	}
	return label
}

// peerDirectionArrow labels which way a peer-sync ran so inbound and
// outbound are distinguishable in the audit trail (friction F18): the
// destination column holds a peer name in both directions. Only the
// initiator records runs.shallow — BeginSyncRunIfClear sets it, while the
// receiver's BeginPeerSyncRun leaves it NULL (see store.Run.Shallow) — so a
// set shallow flag marks the row as the side that pushed. "→" is outbound
// (we pushed to the peer), "←" inbound (the peer pushed to us).
func peerDirectionArrow(r store.Run) string {
	if r.Shallow.Valid {
		return "→"
	}
	return "←"
}

// conflictLabel formats the per-run conflict count for the listing.
// Non-peer-sync runs print "—" so the column doesn't pretend the
// concept is meaningful for index/restore/bucket-sync rows.
func conflictLabel(r store.Run, conflicts map[int64]int) string {
	if r.Kind != store.RunKindSync || !r.PeerNodeID.Valid {
		return "—"
	}
	return fmt.Sprintf("%d", conflicts[r.ID])
}

func destinationLabel(d sql.NullString) string {
	if !d.Valid {
		return "—"
	}
	return d.String
}

func volumeLabel(volumeID sql.NullInt64, volumes map[int64]string) string {
	if !volumeID.Valid {
		// Reserved for cross-volume sync runs once those exist.
		return "(all)"
	}
	if name, ok := volumes[volumeID.Int64]; ok {
		return name
	}
	// Volume row vanished (shouldn't happen given the FK) — surface the id
	// so the user can debug rather than silently rendering nothing.
	return fmt.Sprintf("id=%d", volumeID.Int64)
}

func formatStarted(startedAtNs int64) string {
	return time.Unix(0, startedAtNs).UTC().Format(time.RFC3339)
}

func formatDuration(startedAtNs int64, endedAtNs sql.NullInt64) string {
	if !endedAtNs.Valid {
		return "—"
	}
	d := time.Duration(endedAtNs.Int64 - startedAtNs).Round(time.Millisecond)
	return d.String()
}

// truncateError shortens an error message to a single tabular cell. NULL
// errors render as empty; long messages are cut with an ellipsis so one bad
// run doesn't ruin the layout. The cut is rune-aware so a multibyte
// character cannot be sliced in half (which would print an invalid rune).
func truncateError(errVal sql.NullString) string {
	if !errVal.Valid {
		return ""
	}
	const maxRunes = 60
	runes := []rune(errVal.String)
	if len(runes) <= maxRunes {
		return errVal.String
	}
	return string(runes[:maxRunes-1]) + "…"
}
