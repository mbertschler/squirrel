package config

import (
	"strings"
	"testing"
)

// TestLoadPackedLayoutDefaults: a packed destination with no pack knobs set
// resolves to the layout and applies the three documented defaults.
func TestLoadPackedLayoutDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.cold]
type   = "s3"
provider = "AWS"
bucket = "b"
root   = "p"
layout = "packed"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["cold"]
	if d.Layout != LayoutPacked {
		t.Fatalf("Layout = %q, want %q", d.Layout, LayoutPacked)
	}
	if d.PackThreshold != 32<<20 {
		t.Fatalf("PackThreshold = %d, want %d", d.PackThreshold, 32<<20)
	}
	if d.PackSize != 512<<20 {
		t.Fatalf("PackSize = %d, want %d", d.PackSize, 512<<20)
	}
	if d.ZstdLevel != 3 {
		t.Fatalf("ZstdLevel = %d, want 3", d.ZstdLevel)
	}
}

// TestLoadPackedLayoutExplicitKnobs: explicit human size strings and a zstd
// level parse onto the destination.
func TestLoadPackedLayoutExplicitKnobs(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.cold]
type   = "s3"
provider = "AWS"
bucket = "b"
root   = "p"
layout = "packed"
pack_threshold = "8MiB"
pack_size      = "1GiB"
zstd_level     = 4
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["cold"]
	if d.PackThreshold != 8<<20 {
		t.Fatalf("PackThreshold = %d, want %d", d.PackThreshold, 8<<20)
	}
	if d.PackSize != 1<<30 {
		t.Fatalf("PackSize = %d, want %d", d.PackSize, 1<<30)
	}
	if d.ZstdLevel != 4 {
		t.Fatalf("ZstdLevel = %d, want 4", d.ZstdLevel)
	}
}

// TestLoadNonPackedLayoutZeroesPackKnobs: a mirror destination carries no
// pack knobs — the defaults are applied only for the packed layout.
func TestLoadNonPackedLayoutZeroesPackKnobs(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.plain]
type = "s3"
provider = "AWS"
bucket = "b"
root = "p"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["plain"]
	if d.PackThreshold != 0 || d.PackSize != 0 || d.ZstdLevel != 0 {
		t.Fatalf("pack knobs = (%d, %d, %d), want all zero on a non-packed layout",
			d.PackThreshold, d.PackSize, d.ZstdLevel)
	}
}

func TestLoadRejectsPackedOnLocal(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.scratch]
type   = "local"
root   = "/tmp/dst"
layout = "packed"
`))
	if err == nil || !strings.Contains(err.Error(), "rclone-remote") {
		t.Fatalf("expected rclone-remote requirement error, got %v", err)
	}
}

func TestLoadRejectsPackedOnKopia(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.repo]
type     = "kopia"
root     = "/tmp/repo"
password = "hunter2"
layout   = "packed"
`))
	if err == nil || !strings.Contains(err.Error(), "kopia") {
		t.Fatalf("expected kopia rejection, got %v", err)
	}
}

func TestLoadRejectsBadZstdLevel(t *testing.T) {
	cases := []struct{ name, body string }{
		{"too-low", "zstd_level = 0"},
		{"too-high", "zstd_level = 5"},
		{"string", `zstd_level = "3"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
[destinations.cold]
type   = "s3"
provider = "AWS"
bucket = "b"
root   = "p"
layout = "packed"
`+c.body+"\n"))
			if err == nil || !strings.Contains(err.Error(), "zstd_level") {
				t.Fatalf("err = %v, want zstd_level rejection", err)
			}
		})
	}
}

func TestLoadRejectsBadPackSizes(t *testing.T) {
	cases := []struct{ name, body string }{
		{"threshold-zero", `pack_threshold = "0"`},
		{"threshold-unparseable", `pack_threshold = "big"`},
		{"threshold-non-string", "pack_threshold = 42"},
		{"size-zero", `pack_size = "0B"`},
		{"size-unparseable", `pack_size = "lots"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
[destinations.cold]
type   = "s3"
provider = "AWS"
bucket = "b"
root   = "p"
layout = "packed"
`+c.body+"\n"))
			if err == nil {
				t.Fatalf("expected size rejection, got nil")
			}
			if !strings.Contains(err.Error(), "pack_threshold") && !strings.Contains(err.Error(), "pack_size") {
				t.Fatalf("err = %v, want a pack-size rejection", err)
			}
		})
	}
}

// TestLoadRejectsPackKnobsOnNonPackedLayout: the tuning knobs are confined to
// the packed layout, the same way hash_algo/checkers are confined to their
// types.
func TestLoadRejectsPackKnobsOnNonPackedLayout(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.plain]
type   = "s3"
provider = "AWS"
bucket = "b"
root   = "p"
pack_size = "1GiB"
`))
	if err == nil || !strings.Contains(err.Error(), "pack_size") {
		t.Fatalf("err = %v, want pack_size rejection on a non-packed layout", err)
	}
}
