package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/store"
)

// newDBCmd returns the `squirrel db` parent command. Most subcommands are
// the SQLite-side hygiene primitives that issue #65 wanted shipped as a
// coherent cluster: online backup via VACUUM INTO, integrity check via
// PRAGMA integrity_check, and snapshot-restore for rolling back to a
// known-good copy. `schema` joins them as an inspection primitive — it
// prints the live database's DDL. The migration runner inside
// store.OpenWithOptions also calls Backup automatically before any
// schema-advancing migration; this command group is for the
// operator-facing surface.
func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "SQLite index hygiene: backup, integrity check, restore",
	}
	cmd.AddCommand(newDBBackupCmd())
	cmd.AddCommand(newDBCheckCmd())
	cmd.AddCommand(newDBRestoreCmd())
	cmd.AddCommand(newDBSchemaCmd())
	return cmd
}

func newDBBackupCmd() *cobra.Command {
	var (
		to   string
		keep int
	)
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a consistent online snapshot of the index database",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBBackup(cmd, to, keep)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "snapshot destination path (default: ~/.squirrel/backups/index-<ISO8601>.db)")
	cmd.Flags().IntVar(&keep, "keep", 0, "after writing, rotate the backups directory to keep at most N snapshots (0 means no rotation)")
	return cmd
}

func newDBCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Run PRAGMA integrity_check on the index database",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBCheck(cmd)
		},
	}
	return cmd
}

func newDBRestoreCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "restore <snapshot>",
		Short: "Replace the live index database with a previously taken snapshot",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDBRestore(cmd, args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "skip the running-agent check; required when another process legitimately holds the DB open")
	return cmd
}

func runDBBackup(cmd *cobra.Command, to string, keep int) error {
	cfg, _ := tryLoadConfig(cmd) // cfg may be nil; openStore handles that.
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	dst := to
	if dst == "" {
		// Millisecond precision so back-to-back snapshots (CLI scripts,
		// tests) get distinct filenames without an extra retry loop.
		dst = filepath.Join(defaultBackupsDir(s.Path()), fmt.Sprintf("index-%s.db", time.Now().UTC().Format("20060102T150405.000Z")))
	}
	if err := s.Backup(cmd.Context(), dst); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote snapshot %s\n", dst)
	if keep > 0 {
		removed, err := rotateBackups(filepath.Dir(dst), keep)
		if err != nil {
			return fmt.Errorf("rotate backups: %w", err)
		}
		for _, r := range removed {
			fmt.Fprintf(cmd.OutOrStdout(), "removed older snapshot %s\n", r)
		}
	}
	return nil
}

func runDBCheck(cmd *cobra.Command) error {
	cfg, _ := tryLoadConfig(cmd)
	s, err := openStore(cmd, cfg)
	if err != nil {
		return err
	}
	defer s.Close()

	rows, err := s.IntegrityCheck(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if store.IsIntegrityClean(rows) {
		fmt.Fprintln(out, "ok")
		return nil
	}
	for _, line := range rows {
		fmt.Fprintln(out, line)
	}
	return fmt.Errorf("integrity_check reported %d issue(s); restore from a backup or run SQLite's RECOVER", len(rows))
}

func runDBRestore(cmd *cobra.Command, snapshotPath string, force bool) error {
	cfg, _ := tryLoadConfig(cmd)
	livePath, err := resolveDBPath(cmd, cfg)
	if err != nil {
		return err
	}
	snapshotAbs, err := filepath.Abs(snapshotPath)
	if err != nil {
		return fmt.Errorf("resolve snapshot path: %w", err)
	}
	liveAbs, err := filepath.Abs(livePath)
	if err != nil {
		return fmt.Errorf("resolve live db path: %w", err)
	}
	if snapshotAbs == liveAbs {
		return fmt.Errorf("snapshot path equals live db path; refusing")
	}
	snapshotVer, err := store.PreflightCheckSnapshot(cmd.Context(), snapshotAbs)
	if err != nil {
		return fmt.Errorf("snapshot validation: %w", err)
	}
	if snapshotVer != store.SchemaVersion {
		return fmt.Errorf("snapshot is at schema v%d, binary expects v%d; downgrade the binary or migrate the snapshot first", snapshotVer, store.SchemaVersion)
	}

	if !force {
		// Try to open the live DB exclusively to detect a running
		// agent / concurrent CLI. Skipped under --force so a stuck
		// lockfile (or a deliberately overridden constraint) can
		// be worked around. We split lock-contention from other
		// errors (corruption, permission, invalid DB) so the
		// "in use" hint isn't slapped on diagnostics it doesn't fit.
		err := store.ProbeLiveDBExclusive(cmd.Context(), liveAbs)
		if errors.Is(err, store.ErrLiveDBInUse) {
			return fmt.Errorf("%w (pass --force to skip this check after confirming nothing else holds the DB open)", err)
		}
		if err != nil {
			return fmt.Errorf("probe live db: %w", err)
		}
	}

	preRestore, err := preserveLiveDB(liveAbs)
	if err != nil {
		return err
	}

	// os.Rename is atomic within one filesystem; if the snapshot and the
	// live DB are on different filesystems the rename fails and the user
	// can copy first. preserveLiveDB has already moved the live DB and its
	// sidecars aside, so no stale -wal can attach to the incoming snapshot.
	if err := os.Rename(snapshotAbs, liveAbs); err != nil {
		return fmt.Errorf("replace live DB with snapshot: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "restored %s from %s\n", liveAbs, snapshotAbs)
	if preRestore != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "preserved prior live DB at %s\n", preRestore)
	}
	return nil
}

// preserveLiveDB renames the live database aside to
// "<liveAbs>.pre-restore-<unixnano>" before a restore overwrites it, so a
// wrong restore is recoverable. The -wal/-shm sidecars move with it: this
// keeps the preserved copy's full state (unflushed WAL frames travel
// along) and clears liveAbs of any sidecar before the snapshot lands, so
// a crash between the snapshot rename and a separate cleanup can't replay
// a stale WAL into the restored snapshot. The timestamp follows how
// snapshot filenames are stamped elsewhere. Returns the preserved
// main-file path, or "" when no live DB existed.
func preserveLiveDB(liveAbs string) (string, error) {
	if _, err := os.Stat(liveAbs); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("stat live db: %w", err)
	}
	preRestore := fmt.Sprintf("%s.pre-restore-%d", liveAbs, time.Now().UTC().UnixNano())
	if err := os.Rename(liveAbs, preRestore); err != nil {
		return "", fmt.Errorf("preserve live DB: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Rename(liveAbs+suffix, preRestore+suffix); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("preserve live DB %s sidecar: %w", suffix, err)
		}
	}
	return preRestore, nil
}

// defaultBackupsDir returns the parent directory squirrel uses for its
// own backups, mirroring store.defaultBackupDir but lifted into the CLI
// so subcommands don't have to open the store first to derive it.
// dbPath should be the resolved live database path.
func defaultBackupsDir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "backups")
}

// rotateBackups deletes the oldest snapshots in dir until only `keep`
// remain. Snapshots are identified by the index-* and pre-migration-*
// filename prefixes the store and CLI write — unknown files are left
// alone so we never delete something we didn't put there. This is the
// explicit, operator-driven `db backup --keep` retention, so it does
// include pre-migration-* snapshots; the routine snapshot-on-sync
// rotation (sync.rotateSnapshots) exempts them.
func rotateBackups(dir string, keep int) ([]string, error) {
	if keep <= 0 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type entry struct {
		name    string
		modTime time.Time
	}
	var snaps []entry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "index-") && !strings.HasPrefix(name, "pre-migration-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		snaps = append(snaps, entry{name: name, modTime: info.ModTime()})
	}
	if len(snaps) <= keep {
		return nil, nil
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].modTime.Before(snaps[j].modTime) })
	var removed []string
	for _, s := range snaps[:len(snaps)-keep] {
		p := filepath.Join(dir, s.name)
		if err := os.Remove(p); err != nil {
			return removed, err
		}
		removed = append(removed, p)
	}
	return removed, nil
}
