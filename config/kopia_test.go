package config

import (
	"strings"
	"testing"
)

func TestLoadDestinationKopia(t *testing.T) {
	t.Setenv("REPO_PASSWORD", "hunter2")
	p := writeConfig(t, `
[destinations.mirror]
type     = "kopia"
root     = "/tmp/kopia-repo"
password = { env = "REPO_PASSWORD" }

[volumes.pictures]
path    = "/tmp/pictures"
sync_to = ["mirror"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d, ok := cfg.Destinations["mirror"]
	if !ok {
		t.Fatalf("missing destination")
	}
	if d.Type != "kopia" || d.Root != "/tmp/kopia-repo" {
		t.Fatalf("unexpected destination: %+v", d)
	}
	if d.Params["password"] != "hunter2" {
		t.Fatalf("password not resolved: %v", d.Params)
	}
	// type=kopia drives the kopia binary, so the repository password
	// must never leak into the rendered rclone.conf.
	if d.RcloneSection() != "" {
		t.Fatalf("kopia destination should produce empty rclone section, got:\n%s", d.RcloneSection())
	}
	if got := cfg.Volumes["pictures"].SyncTo; len(got) != 1 || got[0] != "mirror" {
		t.Fatalf("SyncTo = %v, want [mirror]", got)
	}
}

func TestLoadDestinationKopiaPasswordLiteral(t *testing.T) {
	p := writeConfig(t, `
[destinations.mirror]
type     = "kopia"
root     = "/tmp/kopia-repo"
password = "literal-pw"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["mirror"].Params["password"]; got != "literal-pw" {
		t.Fatalf("password = %q, want literal-pw", got)
	}
}

func TestLoadDestinationKopiaRequiresPassword(t *testing.T) {
	p := writeConfig(t, `
[destinations.mirror]
type = "kopia"
root = "/tmp/kopia-repo"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "password is required") {
		t.Fatalf("expected password-required error, got %v", err)
	}
}

func TestLoadRejectsCryptOnKopiaDestination(t *testing.T) {
	p := writeConfig(t, `
[destinations.mirror]
type     = "kopia"
root     = "/tmp/kopia-repo"
password = "pw"

[destinations.mirror.crypt]
password = "obscured-pw"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `type "kopia"`) {
		t.Fatalf("expected crypt-on-kopia rejection, got %v", err)
	}
}

func TestLoadRejectsUnknownFieldOnKopia(t *testing.T) {
	p := writeConfig(t, `
[destinations.mirror]
type     = "kopia"
root     = "/tmp/kopia-repo"
password = "pw"
host     = "nas.local"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown field "host"`) {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}
