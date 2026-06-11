package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCLIDBBackupProducesSnapshot exercises `squirrel db backup`
// against a writable DB and confirms the snapshot file lands at a
// default path (no --to) and opens cleanly via a follow-up command.
func TestCLIDBBackupProducesSnapshot(t *testing.T) {
	f := writeSyncFixture(t)
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)

	out := runCLI(t, "--config", f.configPath, "db", "backup")
	if !strings.Contains(out, "wrote snapshot") {
		t.Fatalf("db backup did not print confirmation:\n%s", out)
	}
	// The snapshot lands under <dbDir>/backups/. Glob for it.
	dbDir := filepath.Dir(f.dbPath)
	matches, _ := filepath.Glob(filepath.Join(dbDir, "backups", "index-*.db"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 backup, got %d: %+v", len(matches), matches)
	}
}

// TestCLIDBBackupRespectsToFlag overrides the default destination
// with --to and confirms the snapshot lands at the chosen path.
func TestCLIDBBackupRespectsToFlag(t *testing.T) {
	f := writeSyncFixture(t)
	dst := filepath.Join(t.TempDir(), "manual.db")
	runCLI(t, "--config", f.configPath, "db", "backup", "--to", dst)
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("snapshot missing at --to path: %v", err)
	}
}

// TestCLIDBBackupKeepRotates confirms --keep N prunes the backups
// directory down to the N most recent snapshots.
func TestCLIDBBackupKeepRotates(t *testing.T) {
	f := writeSyncFixture(t)
	for i := 0; i < 3; i++ {
		dst := filepath.Join(t.TempDir(), "snap-"+itoa(i)+".db")
		runCLI(t, "--config", f.configPath, "db", "backup", "--to", dst)
	}
	// Take three more snapshots into the default dir with --keep 2.
	for i := 0; i < 3; i++ {
		runCLI(t, "--config", f.configPath, "db", "backup", "--keep", "2")
	}
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(f.dbPath), "backups", "index-*.db"))
	if len(matches) > 2 {
		t.Fatalf("rotation kept %d snapshots, want ≤ 2: %+v", len(matches), matches)
	}
}

// TestCLIDBCheckPasses on a clean DB.
func TestCLIDBCheckPasses(t *testing.T) {
	f := writeSyncFixture(t)
	out := runCLI(t, "--config", f.configPath, "db", "check")
	if !strings.Contains(out, "ok") {
		t.Fatalf("db check output: %q, want 'ok'", out)
	}
}

// TestCLIDBRestoreSwapsLiveDB: take a backup, mutate the live DB,
// then restore from the backup. After restore, the live DB reflects
// the snapshot state (no volume row from the mutation).
func TestCLIDBRestoreSwapsLiveDB(t *testing.T) {
	f := writeSyncFixture(t)

	// Snapshot a brand-new DB before any volume rows exist.
	snap := filepath.Join(t.TempDir(), "before.db")
	runCLI(t, "--config", f.configPath, "db", "backup", "--to", snap)

	// Mutate: index the volume, which adds a volume row.
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)

	out := runCLI(t, "--config", f.configPath, "db", "restore", snap)
	if !strings.Contains(out, "restored") {
		t.Fatalf("db restore output: %q", out)
	}

	// volumes listing must now be empty (the snapshot was taken
	// before the index run).
	listing := runCLI(t, "--config", f.configPath, "volumes")
	if strings.Contains(listing, f.volumeName) {
		t.Fatalf("volumes listing still mentions %q after restore:\n%s", f.volumeName, listing)
	}
}

// TestCLIDBRestorePreservesPriorLiveDB is the #111a guard: a restore
// renames the prior live DB aside (recoverable) and prints its path. The
// preserved copy still carries the mutation the snapshot predates, so a
// wrong restore can be rolled back by moving it back.
func TestCLIDBRestorePreservesPriorLiveDB(t *testing.T) {
	f := writeSyncFixture(t)

	snap := filepath.Join(t.TempDir(), "before.db")
	runCLI(t, "--config", f.configPath, "db", "backup", "--to", snap)

	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)

	out := runCLI(t, "--config", f.configPath, "db", "restore", snap)
	preserved := parsePreservedPath(t, out)
	if !strings.HasPrefix(filepath.Base(preserved), "index.db.pre-restore-") {
		t.Fatalf("preserved path %q lacks the pre-restore- stem", preserved)
	}
	if _, err := os.Stat(preserved); err != nil {
		t.Fatalf("preserved prior live DB missing at reported path %q: %v", preserved, err)
	}

	// The preserved copy still has the post-snapshot volume row, so the
	// restore is reversible: swap it back and the volume reappears.
	if err := os.Rename(preserved, f.dbPath); err != nil {
		t.Fatalf("roll back to preserved DB: %v", err)
	}
	listing := runCLI(t, "--config", f.configPath, "volumes")
	if !strings.Contains(listing, f.volumeName) {
		t.Fatalf("rolled-back DB lost the volume row; restore was not reversible:\n%s", listing)
	}
}

// TestCLIDBRestoreClearsLiveSidecarsBeforeRename is the #111b guard: any
// -wal/-shm beside the live DB is moved aside with it before the snapshot
// is renamed in, so no stale WAL can be replayed into the restored
// snapshot. We pre-seed sidecars at the live path and restore with
// --force (the clean-open probe would otherwise checkpoint them away),
// then assert none remain at the live path while the preserved copy
// carries them — proving the clearing happens at the rename, not via the
// probe.
func TestCLIDBRestoreClearsLiveSidecarsBeforeRename(t *testing.T) {
	f := writeSyncFixture(t)

	snap := filepath.Join(t.TempDir(), "before.db")
	runCLI(t, "--config", f.configPath, "db", "backup", "--to", snap)

	// Force the live DB into existence, then plant stale sidecars beside it.
	writeTestFile(t, filepath.Join(f.volumeDir, "a.txt"), "alpha")
	runCLI(t, "--config", f.configPath, "index", f.volumeName)
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(f.dbPath+suffix, []byte("stale"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := runCLI(t, "--config", f.configPath, "db", "restore", "--force", snap)
	preserved := parsePreservedPath(t, out)

	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(f.dbPath + suffix); !os.IsNotExist(err) {
			t.Fatalf("stale %s sidecar still beside live DB after restore (err=%v)", suffix, err)
		}
		if _, err := os.Stat(preserved + suffix); err != nil {
			t.Fatalf("preserved DB missing its %s sidecar: %v", suffix, err)
		}
	}
}

// parsePreservedPath extracts the path from the
// "preserved prior live DB at <path>" restore output line.
func parsePreservedPath(t *testing.T, out string) string {
	t.Helper()
	const marker = "preserved prior live DB at "
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, marker) {
			return strings.TrimSpace(strings.TrimPrefix(line, marker))
		}
	}
	t.Fatalf("restore output did not report a preserved path:\n%s", out)
	return ""
}

// TestCLIDBRestoreRejectsSchemaMismatch covers the safety property:
// a snapshot whose schema_version differs from the binary is refused.
// We simulate this by feeding a file that isn't a squirrel DB at all.
func TestCLIDBRestoreRejectsSchemaMismatch(t *testing.T) {
	f := writeSyncFixture(t)
	bogus := filepath.Join(t.TempDir(), "not-a-db.bin")
	if err := os.WriteFile(bogus, []byte("definitely not sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runCLIExpectErr(t, "--config", f.configPath, "db", "restore", bogus)
	if err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("expected snapshot-validation error, got %v", err)
	}
}

// TestCLIDBSchemaPrintsDDL confirms `squirrel db schema` dumps the
// opened database's DDL, including the invariants the schema enforces:
// the foundational volumes table, the content entity table, and the
// one-live-row-per-path partial unique index.
func TestCLIDBSchemaPrintsDDL(t *testing.T) {
	f := writeSyncFixture(t)
	out := runCLI(t, "--config", f.configPath, "db", "schema")
	for _, want := range []string{
		"CREATE TABLE volumes",
		"CREATE TABLE contents",
		"uniq_files_live_per_path",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("db schema output missing %q:\n%s", want, out)
		}
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	// Minimal-deps int-to-string for the test; standard call would
	// be strconv.Itoa but the test file otherwise has no strconv use.
	const digits = "0123456789"
	var buf [16]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	return string(buf[pos:])
}
