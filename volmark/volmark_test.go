package volmark

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadMissingReturnsSentinel(t *testing.T) {
	root := t.TempDir()
	_, err := Read(root)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("Read on empty dir: want ErrMissing, got %v", err)
	}
}

func TestWriteThenReadRoundTrip(t *testing.T) {
	root := t.TempDir()
	want := Marker{Volume: "pics", Node: "laptop", CreatedAt: "2026-05-22T15:00:00Z"}
	if err := Write(root, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := Read(root)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Volume != want.Volume || got.Node != want.Node || got.CreatedAt != want.CreatedAt {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestWriteRefusesEmptyVolume(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Marker{}); err == nil {
		t.Fatalf("Write with empty Volume should error")
	}
}

func TestWriteRefusesMissingRoot(t *testing.T) {
	if err := Write("/nonexistent/path/that/cannot/exist", Marker{Volume: "x"}); err == nil {
		t.Fatalf("Write to missing root should error")
	}
}

func TestReadRejectsCorruptJSON(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(Path(root), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(root); err == nil {
		t.Fatalf("Read of corrupt marker should error")
	}
}

func TestReadRejectsEmptyVolumeField(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(Path(root), []byte(`{"volume":""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Read(root)
	if err == nil || !strings.Contains(err.Error(), "missing volume field") {
		t.Fatalf("Read of empty-volume marker: want missing-volume error, got %v", err)
	}
}

func TestValidateMatch(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Marker{Volume: "pics"}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(root, "pics"); err != nil {
		t.Fatalf("Validate match: %v", err)
	}
}

func TestValidateMismatchReturnsTypedError(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Marker{Volume: "pics"}); err != nil {
		t.Fatal(err)
	}
	err := Validate(root, "video")
	if err == nil {
		t.Fatalf("Validate mismatch should error")
	}
	var mismatch *ErrMismatch
	if !errors.As(err, &mismatch) {
		t.Fatalf("err type = %T, want *ErrMismatch", err)
	}
	if mismatch.Expected != "video" || mismatch.Found != "pics" {
		t.Fatalf("mismatch fields = %+v, want Expected=video Found=pics", mismatch)
	}
}

func TestValidateMissingPropagatesSentinel(t *testing.T) {
	root := t.TempDir()
	if err := Validate(root, "pics"); !errors.Is(err, ErrMissing) {
		t.Fatalf("Validate on empty dir: want ErrMissing, got %v", err)
	}
}

func TestWriteAtomicReplacesExisting(t *testing.T) {
	root := t.TempDir()
	if err := Write(root, Marker{Volume: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := Write(root, Marker{Volume: "new"}); err != nil {
		t.Fatal(err)
	}
	m, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	if m.Volume != "new" {
		t.Fatalf("Volume = %q, want %q", m.Volume, "new")
	}
	// No tempfile leak after overwrite.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != "" && e.Name() != MarkerName {
			t.Fatalf("stale tempfile %q in root after Write", e.Name())
		}
	}
}
