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

// newAuditCmd returns the `squirrel audit [<volume>] [--deep|--folders]`
// cobra command. By default it is the one-shot equivalent of the agent's
// scheduled drift scan (#17): same code path (an index walk tagged with
// kind='audit'), just driven by the operator instead of a ticker.
//
// `--deep` re-hashes every file unconditionally (today's full-rehash
// path) — useful as a quarterly bit-rot check; the default uses the
// (size, mtime) shortcut equivalent to `squirrel index --shallow`.
//
// `--folders` switches to a pure index self-check: instead of walking
// the disk, it re-derives every folder's shallow + deep Merkle hash from
// the stored file/child rows and reports any folder whose stored digest
// diverged (SAFETY-AUDIT M2). It touches no disk and writes no run; the
// command exits non-zero if any divergence is found.
func newAuditCmd() *cobra.Command {
	var deep, folders bool
	cmd := &cobra.Command{
		Use:   "audit [<volume>]",
		Short: "Walk a volume looking for out-of-band drift since the last index",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			volumeName := ""
			if len(args) == 1 {
				volumeName = args[0]
			}
			if folders {
				if deep {
					return fmt.Errorf("--folders and --deep are mutually exclusive")
				}
				return runFolderAudit(cmd, volumeName)
			}
			return runAudit(cmd, volumeName, deep)
		},
	}
	cmd.Flags().BoolVar(&deep, "deep", false, "re-hash every file (bit-rot check); default uses the size+mtime shortcut")
	cmd.Flags().BoolVar(&folders, "folders", false, "re-derive folder Merkle hashes from the index and report divergence (no disk walk)")
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

// runFolderAudit re-derives every folder's Merkle hashes for the target
// volumes and reports divergences against the stored digests. It returns
// a non-nil error (so the process exits non-zero) when any folder
// diverged, after printing the full divergence set; a clean volume prints
// "consistent". No disk walk, no run row — this is a pure index
// self-check (SAFETY-AUDIT M2).
func runFolderAudit(cmd *cobra.Command, volumeName string) error {
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
	var anyDiverged bool
	for _, name := range names {
		vol, err := s.GetVolumeByName(cmd.Context(), name)
		if err != nil {
			if store.IsNotFound(err) {
				return fmt.Errorf("volume %q has no index yet; run `squirrel index %s` first", name, name)
			}
			return fmt.Errorf("lookup volume %q: %w", name, err)
		}
		diverged, err := s.CheckFolderHashes(cmd.Context(), vol.ID)
		if err != nil {
			return fmt.Errorf("check folder hashes for %q: %w", name, err)
		}
		printFolderAudit(out, name, diverged)
		if len(diverged) > 0 {
			anyDiverged = true
		}
	}
	if anyDiverged {
		return fmt.Errorf("folder hash audit found divergences")
	}
	return nil
}

// printFolderAudit renders one volume's folder self-check result: a
// per-folder line for each divergence (naming which digest diverged and
// the stored vs derived bytes), or a single "consistent" line when the
// volume passed.
func printFolderAudit(out io.Writer, name string, diverged []store.FolderHashDivergence) {
	if len(diverged) == 0 {
		fmt.Fprintf(out, "audit %s --folders: consistent\n", name)
		return
	}
	fmt.Fprintf(out, "audit %s --folders: %d folder(s) diverged\n", name, len(diverged))
	for _, d := range diverged {
		if d.ShallowDiverged {
			fmt.Fprintf(out, "  %s shallow: stored=%x derived=%x\n",
				folderLabel(d.Path), d.StoredShallow, d.DerivedShallow)
		}
		if d.DeepDiverged {
			fmt.Fprintf(out, "  %s deep: stored=%x derived=%x\n",
				folderLabel(d.Path), d.StoredDeep, d.DerivedDeep)
		}
	}
}

// folderLabel renders a folder path for the audit listing, mapping the
// volume root (the empty path) to "(root)" so the line isn't blank.
func folderLabel(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
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
