package main

import (
	"database/sql"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newHooksCmd returns the `squirrel hooks` cobra command. It lists rows
// from the hook_runs table — the generic outcome of each external-tool
// hook squirrel fired on a change or interval trigger (#84) — most recent
// first, with optional volume filtering and a configurable cap. squirrel
// records pass/fail and an exit code only; it never interprets what the
// command did, so the listing is deliberately tool-agnostic.
func newHooksCmd() *cobra.Command {
	var (
		volumeName string
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "List external-tool hook runs (most recent first)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHooks(cmd, volumeName, limit)
		},
	}
	cmd.Flags().StringVar(&volumeName, "volume", "", "filter to hooks for this volume name")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of hook runs to show (0 for no limit)")
	return cmd
}

func runHooks(cmd *cobra.Command, volumeName string, limit int) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	opts := store.HookRunListOpts{Limit: limit, Descending: true}
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
	hooks, err := s.ListHookRuns(cmd.Context(), opts)
	if err != nil {
		return err
	}
	volumes, err := loadVolumeNames(cmd, s)
	if err != nil {
		return err
	}
	return printHookRuns(cmd.OutOrStdout(), hooks, volumes)
}

func printHookRuns(out io.Writer, hooks []store.HookRun, volumes map[int64]string) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tVOLUME\tTRIGGER\tRUN\tCHANGED\tSTARTED\tDURATION\tSTATUS\tEXIT\tERROR")
	for _, h := range hooks {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n",
			h.ID, hookVolumeLabel(h.VolumeID, volumes), h.Trigger,
			triggeringRunLabel(h.TriggeringRunID), h.Changed,
			formatStarted(h.StartedAtNs),
			formatDuration(h.StartedAtNs, h.EndedAtNs),
			h.Status, hookExitLabel(h.ExitCode),
			truncateError(h.Error),
		)
	}
	return tw.Flush()
}

// hookVolumeLabel resolves a hook run's volume_id to its name. Unlike the
// runs listing, hook_runs.volume_id is NOT NULL (a hook is always scoped
// to one volume), so the only fallback is the defensive "id=N" for a row
// whose volume vanished.
func hookVolumeLabel(volumeID int64, volumes map[int64]string) string {
	if name, ok := volumes[volumeID]; ok {
		return name
	}
	return fmt.Sprintf("id=%d", volumeID)
}

// triggeringRunLabel renders the index run that fired an on-change hook.
// Interval hooks have no triggering run, so the column shows "—".
func triggeringRunLabel(runID sql.NullInt64) string {
	if !runID.Valid {
		return "—"
	}
	return fmt.Sprintf("%d", runID.Int64)
}

// hookExitLabel renders the recorded exit code. A timeout or spawn failure
// produced no code, so the column shows "—" and the ERROR column carries
// the reason.
func hookExitLabel(exitCode sql.NullInt64) string {
	if !exitCode.Valid {
		return "—"
	}
	return fmt.Sprintf("%d", exitCode.Int64)
}
