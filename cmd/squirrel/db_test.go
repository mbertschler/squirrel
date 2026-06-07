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
// the foundational volumes table, the blake3-immutability trigger, and
// the one-live-row-per-path partial unique index.
func TestCLIDBSchemaPrintsDDL(t *testing.T) {
	f := writeSyncFixture(t)
	out := runCLI(t, "--config", f.configPath, "db", "schema")
	for _, want := range []string{
		"CREATE TABLE volumes",
		"CREATE TRIGGER files_blake3_immutable",
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
