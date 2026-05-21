package main

import (
	"os"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/tui"
)

// newTUICmd returns the `squirrel tui` cobra command, which opens the
// interactive terminal UI against the local index database. It always opens
// the TUI regardless of whether stdin/stdout look like a TTY — explicit
// invocation overrides the TTY-default heuristic on the root command.
func newTUICmd() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the interactive terminal UI",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTUI(cmd)
		},
	}
}

// runTUI is the shared implementation used by both the explicit `tui`
// subcommand and the bare-root TTY-default fallback in newRootCmd. The
// config is optional: Browse + history work without one, but write actions
// (kick index/sync) need an [agent] block to talk to.
func runTUI(cmd *cobra.Command) error {
	cfg, err := tryLoadConfig(cmd)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()
	return tui.Run(cmd.Context(), s, cfg)
}

// stdinIsTTY reports whether the process's stdin AND stdout are both
// attached to a real terminal. Used by the root command to decide whether
// bare `squirrel` should launch the TUI or print help. We check both ends
// because a TUI that can't read keystrokes (stdin redirected) or render to
// the terminal (stdout piped) is worse than no TUI at all.
func stdinIsTTY() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd())
}
