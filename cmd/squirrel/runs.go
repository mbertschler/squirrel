package main

import (
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
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
	s, err := openStore(cmd)
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
	return printRuns(cmd.OutOrStdout(), runs, volumes)
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

func printRuns(out io.Writer, runs []store.Run, volumes map[int64]string) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tVOLUME\tSTARTED\tDURATION\tSTATUS\tFILES\tERROR")
	for _, r := range runs {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			r.ID, r.Kind, volumeLabel(r.VolumeID, volumes),
			formatStarted(r.StartedAtNs),
			formatDuration(r.StartedAtNs, r.EndedAtNs),
			r.Status, r.FileCount, truncateError(r.Error),
		)
	}
	return tw.Flush()
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
// run doesn't ruin the layout.
func truncateError(errVal sql.NullString) string {
	if !errVal.Valid {
		return ""
	}
	const maxLen = 60
	if len(errVal.String) <= maxLen {
		return errVal.String
	}
	return errVal.String[:maxLen-1] + "…"
}
