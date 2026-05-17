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
// configurable cap. Runs of every status (running, success, failed, partial)
// are surfaced so the user can see in-flight or failed indexing alongside
// successful ones.
func newRunsCmd() *cobra.Command {
	var (
		volumeName string
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "runs",
		Short: "List index runs (most recent first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRuns(cmd, volumeName, limit)
		},
	}
	cmd.Flags().StringVar(&volumeName, "volume", "", "filter to runs against this volume name")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of runs to show (0 for no limit)")
	return cmd
}

func runRuns(cmd *cobra.Command, volumeName string, limit int) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	opts := store.ListRunsOpts{Limit: limit, Descending: true}
	if volumeName != "" {
		v, err := s.GetVolumeByName(cmd.Context(), volumeName)
		if err != nil {
			if store.IsNotFound(err) {
				return fmt.Errorf("no volume named %q", volumeName)
			}
			return fmt.Errorf("lookup volume %q: %w", volumeName, err)
		}
		opts.VolumeID = &v.ID
	}
	runs, err := s.ListRuns(cmd.Context(), opts)
	if err != nil {
		return err
	}
	volumes, err := loadVolumeNames(cmd, s)
	if err != nil {
		return err
	}
	conflicts, err := loadConflictCounts(cmd, s, runs)
	if err != nil {
		return err
	}
	return printRuns(cmd.OutOrStdout(), runs, volumes, conflicts)
}

// loadConflictCounts looks up the conflict count for every sync run in
// the listing. Conflicts are derived from a path-prefix query against
// `files` rather than stored on the run row directly — the v6 schema
// already carries enough information, so we avoided a v7 migration for
// what is essentially a derived audit number. Index/restore runs and
// peer-less sync runs return 0 with no query issued.
func loadConflictCounts(cmd *cobra.Command, s *store.Store, runs []store.Run) (map[int64]int, error) {
	out := make(map[int64]int, len(runs))
	for _, r := range runs {
		if r.Kind != store.RunKindSync || !r.PeerNodeID.Valid {
			continue
		}
		n, err := s.CountFilesFirstSeenWithPathPrefix(cmd.Context(), r.ID, sync.ConflictsDirName)
		if err != nil {
			return nil, fmt.Errorf("conflict count for run %d: %w", r.ID, err)
		}
		out[r.ID] = n
	}
	return out, nil
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

func printRuns(out io.Writer, runs []store.Run, volumes map[int64]string, conflicts map[int64]int) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tVOLUME\tDESTINATION\tSTARTED\tDURATION\tSTATUS\tFILES\tCONFLICTS\tERROR")
	for _, r := range runs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			r.ID, r.Kind, volumeLabel(r.VolumeID, volumes),
			destinationLabel(r.Destination),
			formatStarted(r.StartedAtNs),
			formatDuration(r.StartedAtNs, r.EndedAtNs),
			r.Status, r.FileCount, conflictLabel(r, conflicts),
			truncateError(r.Error),
		)
	}
	return tw.Flush()
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
