package main

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/status"
)

// newStatusCmd returns the `squirrel status [volume]` cobra command: a
// read-only "am I safe?" grid answering, per (volume × target), whether
// every configured target is caught up and whether the volume's local
// content is durable where its offload policy needs it (friction-log F16,
// F17, F23). It mutates nothing. The process exit code reflects the worst
// state — 0 green, 1 amber, 2 red — so the same command scripts a health
// check without parsing the grid.
func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [volume]",
		Short: "Show per-target sync coverage and durability, and the offload-ready total",
		Args:  cobra.MaximumNArgs(1),
		// The amber/red signal is carried out as an exitCodeError; silence
		// cobra's own error printing so the grid is the only output and the
		// signal shows up purely in the exit status.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var volume string
			if len(args) == 1 {
				volume = args[0]
			}
			err := runStatus(cmd, volume)
			// SilenceErrors keeps the scriptable green/amber/red exit-code
			// signal (an exitCodeError) from leaking a cobra "Error:" line. But
			// it also silences genuine failures — an unknown volume, a missing
			// config, a store error — which must still reach the user, so print
			// those here. The exit-code signal stays silent.
			var ec exitCodeError
			if err != nil && !errors.As(err, &ec) {
				fmt.Fprintln(cmd.ErrOrStderr(), cmd.ErrPrefix(), err.Error())
			}
			return err
		},
	}
	return cmd
}

func runStatus(cmd *cobra.Command, volume string) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	if volume != "" {
		if _, ok := cfg.Volumes[volume]; !ok {
			return fmt.Errorf("unknown volume %q (declare it in %s)", volume, cfg.Path)
		}
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rep, err := status.Build(cmd.Context(), s, cfg)
	if err != nil {
		return err
	}
	worst := renderStatus(cmd.OutOrStdout(), rep, volume)
	if code := worst.ExitCode(); code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

// renderStatus prints the grid and returns the worst level across the
// volumes it printed. When volume is non-empty only that volume is shown,
// and only its level drives the return (so a scripted per-volume check
// isn't reddened by an unrelated volume).
func renderStatus(w io.Writer, rep status.Report, volume string) status.Level {
	worst := status.LevelNeutral
	for _, v := range rep.Volumes {
		if volume != "" && v.Name != volume {
			continue
		}
		renderStatusVolume(w, v)
		if v.Level() > worst {
			worst = v.Level()
		}
	}
	fmt.Fprintf(w, "overall: %s\n", status.TrafficLight(worst))
	return worst
}

// renderStatusVolume prints one volume's header, its target grid, and its
// offload-readiness line, reusing the shared status labels so the wording
// matches the TUI exactly.
func renderStatusVolume(w io.Writer, v status.VolumeStatus) {
	fmt.Fprintf(w, "\n%s  %s  [%s]\n", v.Name, v.Path, status.TrafficLight(v.Level()))
	fmt.Fprintf(w, "  index: %s\n", status.IndexLabel(v))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TARGET\tROLE\tLAST SYNC\tSTATE\tDURABLE\tMETHOD\tEVIDENCE")
	for _, t := range v.Targets {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			t.Name, status.RoleLabel(t), status.LastSyncLabel(t), status.StateLabel(t),
			status.DurableLabel(t), status.MethodLabel(t), status.EvidenceLabel(t))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "  %s\n", status.OffloadLabel(v.Offload))
}
