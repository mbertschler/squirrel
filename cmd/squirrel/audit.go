package main

import (
	"fmt"
	"io"
	"sort"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/index"
	"github.com/mbertschler/squirrel/store"
)

// newAuditCmd returns the `squirrel audit [<volume>] [--deep]` cobra
// command. It is the one-shot equivalent of the daemon's scheduled
// drift scan (#17): same code path (an index walk tagged with
// kind='audit'), just driven by the operator instead of a ticker.
//
// `--deep` re-hashes every file unconditionally (today's full-rehash
// path) — useful as a quarterly bit-rot check; the default uses the
// (size, mtime) shortcut equivalent to `squirrel index --shallow`.
func newAuditCmd() *cobra.Command {
	var deep bool
	cmd := &cobra.Command{
		Use:   "audit [<volume>]",
		Short: "Walk a volume looking for out-of-band drift since the last index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeName := ""
			if len(args) == 1 {
				volumeName = args[0]
			}
			return runAudit(cmd, volumeName, deep)
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "re-hash every file (bit-rot check); default uses the size+mtime shortcut")
	return cmd
}

func runAudit(cmd *cobra.Command, volumeName string, deep bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	names, err := auditTargetNames(cfg, volumeName)
	if err != nil {
		return err
	}
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	out := cmd.OutOrStdout()
	var anyError bool
	for _, name := range names {
		vol := cfg.Volumes[name]
		rep, err := index.Index(cmd.Context(), s, vol.Path, index.Options{
			Name:    vol.Name,
			Kind:    store.RunKindAudit,
			Shallow: !deep,
		})
		printAuditReport(out, cmd.ErrOrStderr(), name, rep, err)
		if err != nil || rep.Errors > 0 {
			anyError = true
		}
	}
	if anyError {
		return fmt.Errorf("one or more audit runs encountered errors")
	}
	return nil
}

// auditTargetNames returns the audit subjects in deterministic order.
// An explicit name narrows the walk to one volume (validated against
// config); an empty string fans out to every declared volume, sorted
// by name so log output is stable.
func auditTargetNames(cfg *config.Config, volumeName string) ([]string, error) {
	if volumeName != "" {
		if _, ok := cfg.Volumes[volumeName]; !ok {
			return nil, fmt.Errorf("unknown volume %q (declare it in %s)", volumeName, cfg.Path)
		}
		return []string{volumeName}, nil
	}
	if len(cfg.Volumes) == 0 {
		return nil, fmt.Errorf("no volumes declared in %s", cfg.Path)
	}
	names := make([]string, 0, len(cfg.Volumes))
	for name := range cfg.Volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func printAuditReport(out, errOut io.Writer, name string, rep index.Report, runErr error) {
	for _, e := range rep.ErrorList {
		fmt.Fprintln(errOut, "error:", e)
	}
	fmt.Fprintf(out, "audit %s: run=%d added=%d modified=%d unchanged=%d missing=%d errors=%d\n",
		name, rep.RunID, rep.Added, rep.Modified, rep.Unchanged, rep.Missing, rep.Errors)
	if runErr != nil {
		fmt.Fprintf(errOut, "audit %s: %v\n", name, runErr)
	}
}
