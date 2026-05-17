package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig is a t.TempDir-rooted helper that writes the given TOML body
// to a config.toml file and returns its path. Tests that exercise Load all
// follow the same pattern of "write a config, parse it, assert on the
// resolved struct."
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return p
}

func TestLoadMinimal(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Path != p {
		t.Fatalf("Path = %q, want %q", cfg.Path, p)
	}
	v, ok := cfg.Volumes["pictures"]
	if !ok {
		t.Fatalf("missing volume 'pictures': %#v", cfg.Volumes)
	}
	if v.Path != "/tmp/pictures" {
		t.Fatalf("Path = %q, want /tmp/pictures", v.Path)
	}
	if len(v.SyncTo) != 0 {
		t.Fatalf("SyncTo = %v, want empty", v.SyncTo)
	}
}

func TestLoadMissing(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.toml"))
	if !IsMissing(err) {
		t.Fatalf("expected MissingError, got %v", err)
	}
}

func TestLoadHomeExpansion(t *testing.T) {
	// ~ at the start of a path expands to $HOME; trailing path parts stay
	// intact. We rely on os.UserHomeDir matching $HOME on the test host.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir available")
	}
	p := writeConfig(t, `
[volumes.pictures]
path = "~/Pictures"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := filepath.Join(home, "Pictures")
	if cfg.Volumes["pictures"].Path != want {
		t.Fatalf("Path = %q, want %q", cfg.Volumes["pictures"].Path, want)
	}
}

func TestLoadDestinationLocal(t *testing.T) {
	p := writeConfig(t, `
[destinations.scratch]
type = "local"
root = "/tmp/scratch"

[volumes.pictures]
path = "/tmp/pictures"
sync_to = ["scratch"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d, ok := cfg.Destinations["scratch"]
	if !ok {
		t.Fatalf("missing destination")
	}
	if d.Type != "local" || d.Root != "/tmp/scratch" {
		t.Fatalf("unexpected destination: %+v", d)
	}
	// type=local emits nothing into rclone.conf — addressed by abs path.
	if d.RcloneSection() != "" {
		t.Fatalf("local destination should produce empty rclone section, got:\n%s", d.RcloneSection())
	}
	if got := cfg.Volumes["pictures"].SyncTo; len(got) != 1 || got[0] != "scratch" {
		t.Fatalf("SyncTo = %v, want [scratch]", got)
	}
}

func TestLoadDestinationSFTPWithEnvSecret(t *testing.T) {
	t.Setenv("NAS_PASSWORD", "hunter2")
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/volume1/squirrel"
password = { env = "NAS_PASSWORD" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["nas"]
	if d.Params["password"] != "hunter2" {
		t.Fatalf("password not resolved: %v", d.Params)
	}
	section := d.RcloneSection()
	for _, want := range []string{"[nas]", "type = sftp", "host = nas.local", "user = martin", "password = hunter2"} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
}

func TestLoadDestinationSecretLiteral(t *testing.T) {
	// Plain string secrets are allowed (for users who'd rather paste a
	// credential than wire up an env var). Resolution is a no-op.
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/x"
password = "literal-secret"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Destinations["nas"].Params["password"] != "literal-secret" {
		t.Fatalf("literal password not preserved")
	}
}

func TestLoadDestinationMissingEnv(t *testing.T) {
	// An env reference whose variable is unset must fail at load time —
	// we don't want rclone to fail later with a confusing message.
	os.Unsetenv("DEFINITELY_NOT_SET")
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "h"
user = "u"
root = "/r"
password = { env = "DEFINITELY_NOT_SET" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "DEFINITELY_NOT_SET") {
		t.Fatalf("expected missing-env error, got %v", err)
	}
}

func TestLoadRejectsUnknownDestinationType(t *testing.T) {
	p := writeConfig(t, `
[destinations.weird]
type = "ftp"
root = "/x"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unsupported destination type "ftp"`) {
		t.Fatalf("expected unsupported-type error, got %v", err)
	}
}

func TestLoadRejectsUnknownFieldOnDestination(t *testing.T) {
	// Strict per-type validation: typos must fail loudly. Without this,
	// `passsword = ...` would silently leave the destination credential-less.
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "h"
user = "u"
root = "/r"
typo_field = "oops"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown field "typo_field"`) {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadRejectsUnknownTopLevel(t *testing.T) {
	// Top-level keys are restricted to {db, volumes, destinations}.
	p := writeConfig(t, `
extra = "wat"
[volumes.x]
path = "/x"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "extra") {
		t.Fatalf("expected unknown top-level field error, got %v", err)
	}
}

func TestLoadRejectsMissingRequiredField(t *testing.T) {
	// sftp requires host; absence is a load-time error.
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
user = "u"
root = "/r"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "host is required") {
		t.Fatalf("expected missing host error, got %v", err)
	}
}

func TestLoadRejectsSyncToUnknownDest(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
sync_to = ["does-not-exist"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "unknown destination") {
		t.Fatalf("expected unknown-destination error, got %v", err)
	}
}

func TestLoadRejectsInvalidName(t *testing.T) {
	// Names that wouldn't survive being a filesystem subfolder or an
	// rclone.conf section are rejected at load time.
	p := writeConfig(t, `
[volumes."has space"]
path = "/x"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "invalid volume name") {
		t.Fatalf("expected invalid-name error, got %v", err)
	}
}

func TestLoadDBPath(t *testing.T) {
	p := writeConfig(t, `
db = "/var/db/squirrel.db"
[volumes.x]
path = "/x"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB != "/var/db/squirrel.db" {
		t.Fatalf("DB = %q", cfg.DB)
	}
}

func TestRcloneSectionStableOrdering(t *testing.T) {
	// Same destination rendered twice must produce identical bytes — we
	// rely on this for diffability when regenerating rclone.conf.
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "nas.local"
user = "martin"
root = "/r"
key_file = "~/key"
password = "p"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	a := cfg.Destinations["nas"].RcloneSection()
	b := cfg.Destinations["nas"].RcloneSection()
	if a != b {
		t.Fatalf("non-stable section output:\n%s\n---\n%s", a, b)
	}
}

func TestDefaultPathEnv(t *testing.T) {
	t.Setenv(EnvVar, "/explicit/path")
	got, err := DefaultPath()
	if err != nil {
		t.Fatalf("DefaultPath: %v", err)
	}
	if got != "/explicit/path" {
		t.Fatalf("DefaultPath = %q", got)
	}
}

// TestRcloneSectionRenderingPerType exercises the rclone.conf bodies we
// emit for each supported non-local destination type. Beyond catching
// regressions in the per-type field set, this is the only place we
// verify that S3 and B2 secrets round-trip through env-var resolution
// into the ini output.
func TestRcloneSectionRenderingPerType(t *testing.T) {
	t.Setenv("S3_KEY", "AK123")
	t.Setenv("S3_SECRET", "sssh")
	t.Setenv("B2_KEY_ID", "0001")
	t.Setenv("B2_KEY", "appkey")

	body := `
[destinations.s3]
type              = "s3"
provider          = "AWS"
region            = "eu-central-1"
bucket            = "squirrel"
root              = "/p"
access_key_id     = { env = "S3_KEY" }
secret_access_key = { env = "S3_SECRET" }

[destinations.b2]
type            = "b2"
bucket          = "squirrel"
root            = "/p"
account_id      = { env = "B2_KEY_ID" }
application_key = { env = "B2_KEY" }

[destinations.gcs]
type   = "gcs"
bucket = "squirrel"
root   = "/p"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := []struct {
		name  string
		wants []string
	}{
		{"s3", []string{"[s3]", "type = s3", "provider = AWS", "bucket = squirrel", "region = eu-central-1", "access_key_id = AK123", "secret_access_key = sssh"}},
		{"b2", []string{"[b2]", "type = b2", "bucket = squirrel", "account_id = 0001", "application_key = appkey"}},
		{"gcs", []string{"[gcs]", "type = gcs", "bucket = squirrel"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := cfg.Destinations[c.name].RcloneSection()
			for _, want := range c.wants {
				if !strings.Contains(out, want) {
					t.Fatalf("section missing %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestLoadNodeName checks that the top-level node_name key is parsed and
// surfaced on Config.NodeName for the store to consume on first migration.
func TestLoadNodeName(t *testing.T) {
	p := writeConfig(t, `
node_name = "laptop"

[volumes.pictures]
path = "/tmp/pictures"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "laptop" {
		t.Fatalf("NodeName = %q, want laptop", cfg.NodeName)
	}
}

// TestLoadNodeNameInvalid rejects identifiers that don't match nameRE —
// the same conservative subset volumes and destinations are held to,
// because the same string lands as an identifier in the index DB and
// on the sync wire.
func TestLoadNodeNameInvalid(t *testing.T) {
	p := writeConfig(t, `
node_name = "has spaces"
`)
	if _, err := Load(p); err == nil {
		t.Fatalf("Load accepted invalid node_name; want rejection")
	}
}

// TestLoadNodeNameOptional is the bridge case for users who haven't
// added a node_name yet: the field stays empty and the store falls
// back to os.Hostname() at Open.
func TestLoadNodeNameOptional(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "" {
		t.Fatalf("NodeName = %q, want empty (hostname-fallback path)", cfg.NodeName)
	}
}

func TestMissingErrorWrappingChain(t *testing.T) {
	// MissingError must be detectable both via IsMissing and errors.As so
	// callers can choose either ergonomic form.
	err := &MissingError{Path: "/x"}
	if !IsMissing(err) {
		t.Fatalf("IsMissing should match")
	}
	var m *MissingError
	if !errors.As(err, &m) || m.Path != "/x" {
		t.Fatalf("errors.As should populate Path")
	}
}
