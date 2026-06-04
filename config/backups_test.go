package config

import (
	"strings"
	"testing"
)

// TestBackupsDefaultWhenAbsent: an absent [backups] table resolves to the
// zero-config defaults — on by default, both halves enabled.
func TestBackupsDefaultWhenAbsent(t *testing.T) {
	p := writeConfig(t, `
[volumes.pics]
path = "/tmp/pics"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := DefaultBackups()
	if cfg.Backups != want {
		t.Fatalf("Backups = %+v, want defaults %+v", cfg.Backups, want)
	}
}

// TestBackupsOverrides: present keys override the defaults; omitted keys
// keep their default. dir is expanded to an absolute path.
func TestBackupsOverrides(t *testing.T) {
	p := writeConfig(t, `
[backups]
keep = 3
cloud_keep = 10
dir = "/var/backups/squirrel"

[volumes.pics]
path = "/tmp/pics"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	b := cfg.Backups
	if !b.Enabled || !b.Cloud {
		t.Fatalf("Enabled=%v Cloud=%v, want both true (omitted → default)", b.Enabled, b.Cloud)
	}
	if b.Keep != 3 || b.CloudKeep != 10 {
		t.Fatalf("Keep=%d CloudKeep=%d, want 3/10", b.Keep, b.CloudKeep)
	}
	if b.Dir != "/var/backups/squirrel" {
		t.Fatalf("Dir = %q, want /var/backups/squirrel", b.Dir)
	}
}

// TestBackupsDisabled: enabled=false is distinguishable from "omitted"
// thanks to the pointer field, and turns the whole feature off.
func TestBackupsDisabled(t *testing.T) {
	p := writeConfig(t, `
[backups]
enabled = false

[volumes.pics]
path = "/tmp/pics"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Backups.Enabled {
		t.Fatalf("Enabled = true, want false")
	}
	// Cloud keeps its default; Enabled is the master switch the consumer
	// checks first.
	if !cfg.Backups.Cloud {
		t.Fatalf("Cloud = false, want default true (only enabled was set)")
	}
}

// TestBackupsCloudDisabled: cloud=false keeps the local snapshot on but
// turns off the ride-along.
func TestBackupsCloudDisabled(t *testing.T) {
	p := writeConfig(t, `
[backups]
cloud = false

[volumes.pics]
path = "/tmp/pics"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Backups.Enabled {
		t.Fatalf("Enabled = false, want true")
	}
	if cfg.Backups.Cloud {
		t.Fatalf("Cloud = true, want false")
	}
}

func TestBackupsRejectsNegativeKeep(t *testing.T) {
	p := writeConfig(t, `
[backups]
keep = -1

[volumes.pics]
path = "/tmp/pics"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "keep must be non-negative") {
		t.Fatalf("expected negative-keep error, got %v", err)
	}
}

func TestBackupsRejectsUnknownField(t *testing.T) {
	p := writeConfig(t, `
[backups]
nope = true

[volumes.pics]
path = "/tmp/pics"
`)
	if _, err := Load(p); err == nil {
		t.Fatalf("expected unknown-field error for [backups].nope")
	}
}
