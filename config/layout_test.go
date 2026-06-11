package config

import (
	"strings"
	"testing"
)

func TestLoadDestinationLayoutDefaultsToMirror(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "example"
user = "u"
root = "/data"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["offsite"].Layout; got != LayoutMirror {
		t.Fatalf("Layout = %q, want %q", got, LayoutMirror)
	}
}

func TestLoadDestinationContentAddressedLayout(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "example"
user   = "u"
root   = "/data"
layout = "content-addressed"

[destinations.offsite.crypt]
password = "obscured-pw"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["offsite"]
	if d.Layout != LayoutContentAddressed {
		t.Fatalf("Layout = %q, want %q", d.Layout, LayoutContentAddressed)
	}
	if d.Crypt == nil {
		t.Fatalf("crypt block lost when combined with layout")
	}
}

func TestLoadDestinationExplicitMirrorLayout(t *testing.T) {
	p := writeConfig(t, `
[destinations.scratch]
type   = "local"
root   = "/tmp/dst"
layout = "mirror"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["scratch"].Layout; got != LayoutMirror {
		t.Fatalf("Layout = %q, want %q", got, LayoutMirror)
	}
}

func TestLoadRejectsContentAddressedOnLocal(t *testing.T) {
	p := writeConfig(t, `
[destinations.scratch]
type   = "local"
root   = "/tmp/dst"
layout = "content-addressed"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "rclone-remote") {
		t.Fatalf("expected rclone-remote requirement error, got %v", err)
	}
}

func TestLoadRejectsContentAddressedOnKopia(t *testing.T) {
	p := writeConfig(t, `
[destinations.mirror]
type     = "kopia"
root     = "/tmp/repo"
password = "hunter2"
layout   = "content-addressed"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "kopia") {
		t.Fatalf("expected kopia rejection, got %v", err)
	}
}

func TestLoadRejectsUnknownLayout(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "example"
user   = "u"
root   = "/data"
layout = "object-store"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown layout "object-store"`) {
		t.Fatalf("expected unknown-layout error, got %v", err)
	}
}
