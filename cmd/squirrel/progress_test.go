package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/runevents"
)

// TestProgressEnabledGating pins the gate: an explicit flag wins in both
// directions, and with the flag untouched the decision defers to the TTY
// check (false under `go test`, where stdout is a pipe — so scripted and
// agent-style runs stay quiet by default).
func TestProgressEnabledGating(t *testing.T) {
	newCmd := func() *cobra.Command {
		var progress bool
		c := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().BoolVarP(&progress, "progress", "P", false, "")
		return c
	}

	// Flag untouched: auto path follows stdoutIsTTY() (deterministic
	// regardless of whether the test runs under a real terminal).
	c := newCmd()
	if got := progressEnabled(c, false); got != stdoutIsTTY() {
		t.Errorf("flag untouched should follow stdoutIsTTY() = %v, got %v", stdoutIsTTY(), got)
	}

	// Explicit --progress forces on even without a TTY.
	c = newCmd()
	if err := c.Flags().Set("progress", "true"); err != nil {
		t.Fatal(err)
	}
	if !progressEnabled(c, true) {
		t.Error("explicit --progress should force progress on")
	}

	// Explicit --progress=false forces off.
	c = newCmd()
	if err := c.Flags().Set("progress", "false"); err != nil {
		t.Fatal(err)
	}
	if progressEnabled(c, false) {
		t.Error("explicit --progress=false should force progress off")
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1 << 20, "1.0 MiB"},
		{5*(1<<30) + (1 << 29), "5.5 GiB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHumanRate(t *testing.T) {
	if got := humanRate(0); got != "— /s" {
		t.Errorf("humanRate(0) = %q, want em dash", got)
	}
	if got := humanRate(1 << 20); got != "1.0 MiB/s" {
		t.Errorf("humanRate(1MiB) = %q", got)
	}
}

func TestTruncatePath(t *testing.T) {
	if got := truncatePath("a/b/c.txt", 48); got != "a/b/c.txt" {
		t.Errorf("short path changed: %q", got)
	}
	long := strings.Repeat("x", 100) + "/file.txt"
	got := truncatePath(long, 20)
	if len([]rune(got)) != 20 {
		t.Errorf("truncated len = %d, want 20 (%q)", len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "file.txt") {
		t.Errorf("truncation dropped the tail: %q", got)
	}

	// Multibyte path: truncation must count runes and never split a
	// UTF-8 sequence.
	multi := strings.Repeat("ф", 100) + "/файл.txt"
	gotM := truncatePath(multi, 20)
	if len([]rune(gotM)) != 20 {
		t.Errorf("multibyte truncated rune-len = %d, want 20 (%q)", len([]rune(gotM)), gotM)
	}
	if !utf8.ValidString(gotM) {
		t.Errorf("truncation split a UTF-8 sequence: %q", gotM)
	}
	if !strings.HasSuffix(gotM, "файл.txt") {
		t.Errorf("multibyte truncation dropped the tail: %q", gotM)
	}
}

// TestFormatHashingReportsCountsAndPath verifies the index-side line surfaces
// file count, bytes, and the current path (no total, since indexing has none).
func TestFormatHashingReportsCountsAndPath(t *testing.T) {
	pp := newProgressPrinter(&bytes.Buffer{})
	pp.start = time.Now().Add(-time.Second) // one second elapsed for a stable rate
	line := pp.format(runevents.Progress{
		Stage:     runevents.StageHashing,
		Done:      42,
		BytesDone: 1 << 20,
		Message:   "photos/img.jpg",
	})
	for _, want := range []string{"indexing:", "42 files", "1.0 MiB", "photos/img.jpg"} {
		if !strings.Contains(line, want) {
			t.Errorf("hashing line %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "ETA") {
		t.Errorf("hashing line should carry no ETA: %q", line)
	}
}

// TestFormatUploadingReportsETA verifies the sync-side line surfaces
// done/total files and bytes and computes an ETA once a byte total is known.
func TestFormatUploadingReportsETA(t *testing.T) {
	pp := newProgressPrinter(&bytes.Buffer{})
	pp.start = time.Now().Add(-time.Second)
	line := pp.format(runevents.Progress{
		Stage:      runevents.StageUploading,
		Done:       3,
		Total:      10,
		BytesDone:  1 << 20,
		BytesTotal: 4 << 20,
	})
	for _, want := range []string{"syncing:", "3/10 files", "1.0 MiB/4.0 MiB", "ETA"} {
		if !strings.Contains(line, want) {
			t.Errorf("uploading line %q missing %q", line, want)
		}
	}
}

// TestFormatUploadingNoTotalOmitsETA verifies that before rclone announces a
// byte total, no bogus ETA is rendered.
func TestFormatUploadingNoTotalOmitsETA(t *testing.T) {
	pp := newProgressPrinter(&bytes.Buffer{})
	pp.start = time.Now().Add(-time.Second)
	line := pp.format(runevents.Progress{
		Stage:     runevents.StageUploading,
		Done:      3,
		BytesDone: 1 << 20,
	})
	if strings.Contains(line, "ETA") {
		t.Errorf("no total known, but ETA rendered: %q", line)
	}
}

// TestClearErasesLine confirms update writes a carriage-return line and clear
// erases it, so the following summary starts clean.
func TestClearErasesLine(t *testing.T) {
	var buf bytes.Buffer
	pp := newProgressPrinter(&buf)
	pp.update(runevents.Progress{Stage: runevents.StageHashing, Done: 1})
	if !strings.HasPrefix(buf.String(), "\r") {
		t.Errorf("update did not start with carriage return: %q", buf.String())
	}
	pp.clear()
	if !strings.HasSuffix(buf.String(), "\r") {
		t.Errorf("clear did not end on a carriage return: %q", buf.String())
	}
}

// TestClearBeforeUpdateNoOp confirms clearing a printer that never rendered is
// a no-op — nothing is written, so a non-progress run stays byte-for-byte the
// same on stderr.
func TestClearBeforeUpdateNoOp(t *testing.T) {
	var buf bytes.Buffer
	pp := newProgressPrinter(&buf)
	pp.clear()
	if buf.Len() != 0 {
		t.Errorf("clear before any update wrote %q", buf.String())
	}
}
