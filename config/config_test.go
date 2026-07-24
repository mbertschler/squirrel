package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestLoadOffloadRequires: the per-volume offload policy is parsed
// verbatim, including names with no matching local destination or node
// — durability evidence for such targets can be peer-pulled, and an
// unknown name fails closed at the gate.
func TestLoadOffloadRequires(t *testing.T) {
	p := writeConfig(t, `
[destinations.scratch]
type = "local"
root = "/tmp/dst"

[volumes.pictures]
path = "/tmp/pictures"
sync_to = ["scratch"]
offload_requires = ["scratch", "peer-only-offsite"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Volumes["pictures"].OffloadRequires
	if len(got) != 2 || got[0] != "scratch" || got[1] != "peer-only-offsite" {
		t.Fatalf("OffloadRequires = %v, want [scratch peer-only-offsite]", got)
	}
}

func TestLoadRejectsInvalidOffloadRequiresName(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
offload_requires = ["has space"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "offload_requires entry") {
		t.Fatalf("expected invalid offload_requires error, got %v", err)
	}
}

func TestLoadRejectsDuplicateOffloadRequires(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
offload_requires = ["nas", "nas"]
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("expected duplicate offload_requires error, got %v", err)
	}
}

// TestLoadOffloadMaxEvidenceAge: the optional staleness knob parses as a
// duration; absent it defaults to zero (the time-based policy disabled).
func TestLoadOffloadMaxEvidenceAge(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
offload_requires = ["nas"]
offload_max_evidence_age = "720h"

[volumes.docs]
path = "/tmp/docs"
offload_requires = ["nas"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Volumes["pictures"].OffloadMaxEvidenceAge; got != 720*time.Hour {
		t.Fatalf("OffloadMaxEvidenceAge = %s, want 720h", got)
	}
	if got := cfg.Volumes["docs"].OffloadMaxEvidenceAge; got != 0 {
		t.Fatalf("OffloadMaxEvidenceAge = %s, want 0 (disabled by default)", got)
	}
}

func TestLoadRejectsBadOffloadMaxEvidenceAge(t *testing.T) {
	cases := []struct{ name, value string }{
		{"garbage", "soon"},
		{"sub_second", "500ms"},
		{"unitless", "30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeConfig(t, `
[volumes.pictures]
path = "/tmp/pictures"
offload_requires = ["nas"]
offload_max_evidence_age = "`+c.value+`"
`)
			if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "offload_max_evidence_age") {
				t.Fatalf("expected offload_max_evidence_age error, got %v", err)
			}
		})
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

// TestLoadDestinationS3StorageClass parses the optional s3 storage_class and
// confirms it renders verbatim into the s3 section.
func TestLoadDestinationS3StorageClass(t *testing.T) {
	p := writeConfig(t, `
[destinations.archive]
type          = "s3"
provider      = "AWS"
bucket        = "squirrel"
root          = "/p"
storage_class = "DEEP_ARCHIVE"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["archive"]
	if d.Params["storage_class"] != "DEEP_ARCHIVE" {
		t.Fatalf("storage_class not resolved: %v", d.Params)
	}
	if !strings.Contains(d.RcloneSection(), "storage_class = DEEP_ARCHIVE") {
		t.Fatalf("section missing storage_class:\n%s", d.RcloneSection())
	}
}

// TestLoadRejectsStorageClassOnNonS3 confirms storage_class is confined to
// the s3 type by the unknown-field check — it has no meaning on an sftp
// destination.
func TestLoadRejectsStorageClassOnNonS3(t *testing.T) {
	p := writeConfig(t, `
[destinations.nas]
type          = "sftp"
host          = "h"
user          = "u"
root          = "/r"
storage_class = "GLACIER"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown field "storage_class"`) {
		t.Fatalf("expected storage_class rejected on sftp, got %v", err)
	}
}

// TestLoadDestinationSFTPHostKeyValidation parses the optional sftp
// known_hosts_file and host_key_algorithms params and confirms both render
// verbatim into the sftp section. Pointing rclone at a known_hosts file is
// what turns on server host-key validation; absent, rclone accepts any host
// key the server presents.
func TestLoadDestinationSFTPHostKeyValidation(t *testing.T) {
	p := writeConfig(t, `
[destinations.nas]
type                = "sftp"
host                = "h"
user                = "u"
root                = "/r"
password            = "p"
known_hosts_file    = "~/.ssh/known_hosts"
host_key_algorithms = "ssh-ed25519 ssh-rsa"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["nas"]
	if d.Params["known_hosts_file"] != "~/.ssh/known_hosts" {
		t.Fatalf("known_hosts_file not resolved: %v", d.Params)
	}
	if d.Params["host_key_algorithms"] != "ssh-ed25519 ssh-rsa" {
		t.Fatalf("host_key_algorithms not resolved: %v", d.Params)
	}
	section := d.RcloneSection()
	for _, want := range []string{
		"known_hosts_file = ~/.ssh/known_hosts",
		"host_key_algorithms = ssh-ed25519 ssh-rsa",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("section missing %q:\n%s", want, section)
		}
	}
}

// TestLoadRejectsKnownHostsFileOnNonSFTP confirms the host-key params are
// confined to the sftp type by the unknown-field check.
func TestLoadRejectsKnownHostsFileOnNonSFTP(t *testing.T) {
	p := writeConfig(t, `
[destinations.s3]
type             = "s3"
provider         = "AWS"
bucket           = "squirrel"
root             = "/p"
known_hosts_file = "~/.ssh/known_hosts"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown field "known_hosts_file"`) {
		t.Fatalf("expected known_hosts_file rejected on s3, got %v", err)
	}
}

// TestLoadDestinationCrypt parses a crypt block with one env-resolved and
// one literal password, the same secret forms destination credentials
// accept.
func TestLoadDestinationCrypt(t *testing.T) {
	t.Setenv("CRYPT_PASSWORD", "obscured-pw")
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
password = "transport-pw"

[destinations.offsite.crypt]
password  = { env = "CRYPT_PASSWORD" }
password2 = "obscured-salt"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["offsite"]
	if d.Crypt == nil {
		t.Fatalf("Crypt not parsed: %+v", d)
	}
	if d.Crypt.Password != "obscured-pw" || d.Crypt.Password2 != "obscured-salt" {
		t.Fatalf("Crypt = %+v, want resolved password + literal salt", d.Crypt)
	}
	if d.CryptRemoteName() != "offsite-crypt" {
		t.Fatalf("CryptRemoteName = %q, want offsite-crypt", d.CryptRemoteName())
	}
}

// TestRcloneSectionCryptStacked pins the exact two-section render for a
// crypt destination: the underlying remote exactly as without crypt, then
// the overlay wrapping it at the destination root with the fixed
// filename-encryption settings.
func TestRcloneSectionCryptStacked(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
password = "transport-pw"

[destinations.offsite.crypt]
password  = "obscured-pw"
password2 = "obscured-salt"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := `[offsite]
type = sftp
host = host.example
user = u
blake3sum_command = b3sum
password = transport-pw

[offsite-crypt]
type = crypt
remote = offsite:/data
filename_encryption = off
directory_name_encryption = false
password = obscured-pw
password2 = obscured-salt
`
	if got := cfg.Destinations["offsite"].RcloneSection(); got != want {
		t.Fatalf("RcloneSection:\n%s\nwant:\n%s", got, want)
	}
}

// TestRcloneSectionSFTPEmitsBlake3sumCommand pins that every sftp section
// carries a blake3sum_command. rclone never autodetects one, so without it
// squirrel's `--hash blake3` syncs fail with "hash type not supported". The
// line is sftp-only: backends with a fixed provider checksum must not get it.
func TestRcloneSectionSFTPEmitsBlake3sumCommand(t *testing.T) {
	p := writeConfig(t, `
[destinations.nas]
type = "sftp"
host = "h"
user = "u"
root = "/r"

[destinations.s3]
type     = "s3"
provider = "AWS"
bucket   = "b"
root     = "/r"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["nas"].RcloneSection(); !strings.Contains(got, "blake3sum_command = b3sum") {
		t.Fatalf("sftp section missing blake3sum_command:\n%s", got)
	}
	if got := cfg.Destinations["s3"].RcloneSection(); strings.Contains(got, "blake3sum_command") {
		t.Fatalf("non-sftp section should not carry blake3sum_command:\n%s", got)
	}
}

// TestRcloneSectionCryptOmitsEmptySalt: password2 is optional, mirroring
// rclone's own crypt config, and an absent salt renders no password2 line.
func TestRcloneSectionCryptOmitsEmptySalt(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/data"

[destinations.offsite.crypt]
password = "obscured-pw"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	section := cfg.Destinations["offsite"].RcloneSection()
	if strings.Contains(section, "password2") {
		t.Fatalf("section has a password2 line for an absent salt:\n%s", section)
	}
	if !strings.Contains(section, "password = obscured-pw") {
		t.Fatalf("section missing crypt password:\n%s", section)
	}
}

func TestLoadDestinationCryptMissingPassword(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/r"

[destinations.offsite.crypt]
password2 = "salt-only"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "crypt.password is required") {
		t.Fatalf("expected crypt.password-required error, got %v", err)
	}
}

func TestLoadRejectsCryptOnLocalDestination(t *testing.T) {
	p := writeConfig(t, `
[destinations.scratch]
type = "local"
root = "/tmp/scratch"

[destinations.scratch.crypt]
password = "obscured-pw"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `type "local"`) {
		t.Fatalf("expected crypt-on-local rejection, got %v", err)
	}
}

// TestLoadRejectsUnknownCryptField doubles as the "filename encryption is
// fixed, not configurable" pin: a user trying to switch it on gets a
// load-time error.
func TestLoadRejectsUnknownCryptField(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/r"

[destinations.offsite.crypt]
password            = "obscured-pw"
filename_encryption = "standard"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `unknown field "filename_encryption"`) {
		t.Fatalf("expected unknown-crypt-field error, got %v", err)
	}
}

// TestLoadRejectsCryptRemoteNameCollision: the overlay's rclone.conf
// section is named <dest>-crypt, so a sibling destination already holding
// that name would render two sections under one header.
func TestLoadRejectsCryptRemoteNameCollision(t *testing.T) {
	p := writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/r"

[destinations.offsite.crypt]
password = "obscured-pw"

[destinations.offsite-crypt]
type = "local"
root = "/tmp/x"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), `crypt remote name "offsite-crypt"`) {
		t.Fatalf("expected crypt-name collision error, got %v", err)
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

func TestLoadAgentBlock(t *testing.T) {
	t.Setenv("SQUIRREL_AGENT_TOKEN", "s3cret")
	p := writeConfig(t, `
[agent]
listen = "0.0.0.0:8443"
db     = "/var/db/squirrel.db"
tls    = { cert = "/etc/squirrel/cert.pem", key = "/etc/squirrel/key.pem" }
auth   = { token = { env = "SQUIRREL_AGENT_TOKEN" } }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent == nil {
		t.Fatalf("Agent block not parsed")
	}
	if cfg.Agent.Listen != "0.0.0.0:8443" {
		t.Fatalf("Listen = %q", cfg.Agent.Listen)
	}
	if cfg.Agent.DB != "/var/db/squirrel.db" {
		t.Fatalf("DB = %q", cfg.Agent.DB)
	}
	if cfg.Agent.TLSCert != "/etc/squirrel/cert.pem" || cfg.Agent.TLSKey != "/etc/squirrel/key.pem" {
		t.Fatalf("TLS pair = %q / %q", cfg.Agent.TLSCert, cfg.Agent.TLSKey)
	}
	if cfg.Agent.Token != "s3cret" {
		t.Fatalf("Token = %q, want resolved literal", cfg.Agent.Token)
	}
}

func TestLoadAgentMinimalNoTLS(t *testing.T) {
	// No `[agent.tls]` is the plain-HTTP path: TLSCert/TLSKey both empty.
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
auth   = { token = "literal-token" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent == nil {
		t.Fatalf("Agent block not parsed")
	}
	if cfg.Agent.TLSCert != "" || cfg.Agent.TLSKey != "" {
		t.Fatalf("expected no TLS, got %q / %q", cfg.Agent.TLSCert, cfg.Agent.TLSKey)
	}
	if cfg.Agent.Token != "literal-token" {
		t.Fatalf("Token = %q", cfg.Agent.Token)
	}
}

func TestLoadAgentPeerTokens(t *testing.T) {
	t.Setenv("NAS_TOKEN", "nas-secret")
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"

[agent.auth]
token = "shared-fallback"

[agent.auth.peers.laptop]
bearer = "laptop-secret"

[agent.auth.peers.nas]
bearer = { env = "NAS_TOKEN" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := map[string]string{"laptop-secret": "laptop", "nas-secret": "nas"}
	if got := cfg.Agent.PeerTokens; len(got) != len(want) {
		t.Fatalf("PeerTokens = %v, want %v", got, want)
	}
	for token, node := range want {
		if cfg.Agent.PeerTokens[token] != node {
			t.Fatalf("PeerTokens[%q] = %q, want %q", token, cfg.Agent.PeerTokens[token], node)
		}
	}
}

func TestLoadAgentPeerTokensAbsentLeavesNil(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
auth   = { token = "only-shared" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.PeerTokens != nil {
		t.Fatalf("PeerTokens = %v, want nil when no peers configured", cfg.Agent.PeerTokens)
	}
}

func TestLoadAgentPeerTokensRejectsCollisions(t *testing.T) {
	cases := map[string]struct {
		toml string
		want string
	}{
		"duplicate across peers": {
			toml: `
[agent]
listen = "127.0.0.1:9000"
[agent.auth]
token = "shared"
[agent.auth.peers.a]
bearer = "same"
[agent.auth.peers.b]
bearer = "same"
`,
			want: "reuses the token",
		},
		"collides with shared token": {
			toml: `
[agent]
listen = "127.0.0.1:9000"
[agent.auth]
token = "shared"
[agent.auth.peers.a]
bearer = "shared"
`,
			want: "auth.token",
		},
		"empty bearer": {
			toml: `
[agent]
listen = "127.0.0.1:9000"
[agent.auth]
token = "shared"
[agent.auth.peers.a]
bearer = ""
`,
			want: "must not be empty",
		},
		"invalid node name": {
			toml: `
[agent]
listen = "127.0.0.1:9000"
[agent.auth]
token = "shared"
[agent.auth.peers."bad/name"]
bearer = "x"
`,
			want: "invalid node name",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, tc.toml))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func TestLoadAgentMissingToken(t *testing.T) {
	// auth = { } without a token must fail — an open agent port is a
	// footgun even in lab setups, so we refuse to start one.
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
auth   = { }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "auth.token is required") {
		t.Fatalf("expected auth.token-required error, got %v", err)
	}
}

func TestLoadAgentMissingListen(t *testing.T) {
	p := writeConfig(t, `
[agent]
auth = { token = "x" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "listen is required") {
		t.Fatalf("expected listen-required error, got %v", err)
	}
}

func TestLoadAgentPartialTLS(t *testing.T) {
	// One half of the cert/key pair must imply the other — partial TLS is
	// almost certainly a typo.
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
tls    = { cert = "/x.pem" }
auth   = { token = "t" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "tls.cert and tls.key must be set together") {
		t.Fatalf("expected partial-TLS error, got %v", err)
	}
}

func TestLoadAgentRejectsUnknownField(t *testing.T) {
	// Strict per-field validation against typos in the [agent] block.
	p := writeConfig(t, `
[agent]
listen      = "127.0.0.1:9000"
auth        = { token = "t" }
lisetnaddr  = "oops"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "lisetnaddr") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadAgentAbsent(t *testing.T) {
	// Config without [agent] is fine — only the agent subcommand needs it.
	p := writeConfig(t, `
[volumes.x]
path = "/x"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent != nil {
		t.Fatalf("Agent should be nil when block is absent")
	}
}

// TestLoadAgentScanInterval parses the optional drift-detection
// knobs and verifies (a) duration string parses round-trip,
// (b) absent ScanInterval leaves the field zero (scheduler disabled),
// and (c) ScanStrategy defaults to "shallow" when unspecified.
func TestLoadAgentScanInterval(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen        = "127.0.0.1:9000"
auth          = { token = "t" }
scan_interval = "30m"
scan_strategy = "deep"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ScanInterval != 30*time.Minute {
		t.Fatalf("ScanInterval = %s, want 30m", cfg.Agent.ScanInterval)
	}
	if cfg.Agent.ScanStrategy != ScanStrategyDeep {
		t.Fatalf("ScanStrategy = %q, want %q", cfg.Agent.ScanStrategy, ScanStrategyDeep)
	}
}

func TestLoadAgentScanDefaults(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
auth   = { token = "t" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.ScanInterval != 0 {
		t.Fatalf("default ScanInterval = %s, want 0", cfg.Agent.ScanInterval)
	}
	if cfg.Agent.ScanStrategy != ScanStrategyShallow {
		t.Fatalf("default ScanStrategy = %q, want %q", cfg.Agent.ScanStrategy, ScanStrategyShallow)
	}
}

func TestLoadAgentRejectsBadScanStrategy(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen        = "127.0.0.1:9000"
auth          = { token = "t" }
scan_strategy = "fancy"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "scan_strategy") {
		t.Fatalf("expected scan_strategy error, got %v", err)
	}
}

func TestLoadAgentRejectsBadScanInterval(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen        = "127.0.0.1:9000"
auth          = { token = "t" }
scan_interval = "not-a-duration"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "scan_interval") {
		t.Fatalf("expected scan_interval error, got %v", err)
	}
}

// TestLoadNodeBlock parses a complete [nodes.X] block including TLS
// pin and env-resolved bearer. The resolved Endpoint must be a
// fully-parsed *url.URL (not a string) so subsequent layers compose
// per-endpoint URIs via ResolveReference rather than concatenation.
func TestLoadNodeBlock(t *testing.T) {
	t.Setenv("NAS_TOKEN", "supersecret")
	p := writeConfig(t, `
[nodes.nas]
endpoint = "https://nas.local:8443"
path     = "/srv/squirrel"
auth     = { bearer = { env = "NAS_TOKEN" } }
tls      = { cert_fingerprint = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n, ok := cfg.Nodes["nas"]
	if !ok {
		t.Fatalf("nodes.nas missing: %+v", cfg.Nodes)
	}
	if n.Endpoint.Scheme != "https" || n.Endpoint.Host != "nas.local:8443" {
		t.Fatalf("Endpoint = %+v", n.Endpoint)
	}
	if n.Token != "supersecret" {
		t.Fatalf("Token = %q, want resolved literal", n.Token)
	}
	if n.Path != "/srv/squirrel" {
		t.Fatalf("Path = %q", n.Path)
	}
	if !strings.HasPrefix(n.CertFingerprint, "sha256:") || len(n.CertFingerprint) != len("sha256:")+64 {
		t.Fatalf("CertFingerprint = %q", n.CertFingerprint)
	}
}

// TestLoadNodeBlockMinimal: bearer literal, no TLS pin, http endpoint.
func TestLoadNodeBlockMinimal(t *testing.T) {
	p := writeConfig(t, `
[nodes.lan]
endpoint = "http://10.0.0.1:8000"
path     = "/data"
auth     = { bearer = "literal-bearer" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	n := cfg.Nodes["lan"]
	if n.Token != "literal-bearer" {
		t.Fatalf("Token = %q", n.Token)
	}
	if n.CertFingerprint != "" {
		t.Fatalf("CertFingerprint = %q, want empty (no tls block)", n.CertFingerprint)
	}
	if n.DedupStrategy != "copy" {
		t.Fatalf("DedupStrategy = %q, want %q (default)", n.DedupStrategy, "copy")
	}
}

// TestLoadNodeDedupStrategy covers the three resolved outcomes: an
// explicit "copy" passes through, an explicit "off" disables the
// dedup branch, and an unknown value is rejected at load time so a
// typo surfaces before the first sync.
func TestLoadNodeDedupStrategy(t *testing.T) {
	for _, c := range []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{"explicit-copy", `dedup_strategy = "copy"`, "copy", ""},
		{"explicit-off", `dedup_strategy = "off"`, "off", ""},
		{"unknown", `dedup_strategy = "hardlink"`, "", "dedup_strategy"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, `
[nodes.lan]
endpoint = "http://lan.local"
path     = "/r"
auth     = { bearer = "t" }
`+c.body))
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("error = %v, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got := cfg.Nodes["lan"].DedupStrategy; got != c.want {
				t.Fatalf("DedupStrategy = %q, want %q", got, c.want)
			}
		})
	}
}

// TestLoadNodeRejectsBadEndpoint covers the parse + scheme + host
// guards on the endpoint field — a misconfigured URL must surface
// at load time, not first sync.
func TestLoadNodeRejectsBadEndpoint(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing", `[nodes.x]
path = "/r"
auth = { bearer = "t" }`, "endpoint is required"},
		{"bad scheme", `[nodes.x]
endpoint = "ftp://x"
path     = "/r"
auth     = { bearer = "t" }`, "scheme must be http or https"},
		{"no host", `[nodes.x]
endpoint = "http://"
path     = "/r"
auth     = { bearer = "t" }`, "host is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want substring %q", err, c.want)
			}
		})
	}
}

// TestLoadNodeRejectsBadFingerprint checks that the fingerprint must
// match the exact `sha256:<64-hex>` form. A typo'd pin would
// otherwise silently accept any cert.
func TestLoadNodeRejectsBadFingerprint(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint = "https://nas.local"
path     = "/r"
auth     = { bearer = "t" }
tls      = { cert_fingerprint = "sha256:tooshort" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "sha256:<64-hex>") {
		t.Fatalf("error = %v, want fingerprint-shape error", err)
	}
}

// TestLoadVolumeSyncToAcceptsNodeName lets sync_to reference a name
// from either Nodes or Destinations — the user-facing namespace is
// flat.
func TestLoadVolumeSyncToAcceptsNodeName(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint = "http://nas.local"
path     = "/r"
auth     = { bearer = "t" }

[destinations.offsite]
type = "local"
root = "/o"

[volumes.pictures]
path    = "/p"
sync_to = ["nas", "offsite"]
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Volumes["pictures"].SyncTo; len(got) != 2 || got[0] != "nas" || got[1] != "offsite" {
		t.Fatalf("SyncTo = %v", got)
	}
}

// TestLoadRejectsCollidingNodeAndDestinationName guards the "flat
// namespace" rule: nodes and destinations share the sync_to space,
// so a name can be in at most one of them.
func TestLoadRejectsCollidingNodeAndDestinationName(t *testing.T) {
	p := writeConfig(t, `
[nodes.shared]
endpoint = "http://x"
path     = "/r"
auth     = { bearer = "t" }

[destinations.shared]
type = "local"
root = "/o"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "also declared as a destination") {
		t.Fatalf("error = %v, want collision error", err)
	}
}

// TestLoadVolumeCadenceSyncOnly covers `sync_every` standalone:
// IndexEvery stays zero because the scheduler issue will treat that
// as "the pre-sync indexing is enough".
func TestLoadVolumeCadenceSyncOnly(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path       = "/p"
sync_every = "1h"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["pictures"]
	if v.SyncEvery != time.Hour {
		t.Fatalf("SyncEvery = %s, want 1h", v.SyncEvery)
	}
	if v.IndexEvery != 0 {
		t.Fatalf("IndexEvery = %s, want 0 (unset)", v.IndexEvery)
	}
}

// TestLoadVolumeCadenceIndexOnly covers `index_every` standalone — a
// volume that is never auto-synced but is periodically re-indexed
// for forensic history.
func TestLoadVolumeCadenceIndexOnly(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path        = "/p"
index_every = "15m"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["pictures"]
	if v.IndexEvery != 15*time.Minute {
		t.Fatalf("IndexEvery = %s, want 15m", v.IndexEvery)
	}
	if v.SyncEvery != 0 {
		t.Fatalf("SyncEvery = %s, want 0 (unset)", v.SyncEvery)
	}
}

// TestLoadVolumeCadenceBoth covers the canonical happy-path combo
// from the issue example: index between syncs, sync hourly.
func TestLoadVolumeCadenceBoth(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path        = "/p"
sync_every  = "1h"
index_every = "15m"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["pictures"]
	if v.SyncEvery != time.Hour || v.IndexEvery != 15*time.Minute {
		t.Fatalf("cadence = %s / %s, want 1h / 15m", v.SyncEvery, v.IndexEvery)
	}
}

// TestLoadVolumeCadenceDefaults: omitting both leaves them zero, the
// "manual only — agent does not auto-trigger" mode.
func TestLoadVolumeCadenceDefaults(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path = "/p"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	v := cfg.Volumes["pictures"]
	if v.SyncEvery != 0 || v.IndexEvery != 0 {
		t.Fatalf("default cadence = %s / %s, want 0 / 0", v.SyncEvery, v.IndexEvery)
	}
}

// TestLoadVolumeCadenceRejectsIndexNotStrictlyShorter covers the
// strict inequality at the heart of the issue: index_every == or >
// sync_every is meaningless given pre-sync indexing.
func TestLoadVolumeCadenceRejectsIndexNotStrictlyShorter(t *testing.T) {
	cases := []struct{ name, sync, index string }{
		{"equal", "30m", "30m"},
		{"longer", "10m", "1h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeConfig(t, `
[volumes.pictures]
path        = "/p"
sync_every  = "`+c.sync+`"
index_every = "`+c.index+`"
`)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), "index_every must be strictly shorter than sync_every") {
				t.Fatalf("error = %v, want strict-inequality error", err)
			}
		})
	}
}

// TestLoadVolumeCadenceRejectsNonPositive: zero/negative durations
// are configuration errors (the absent-key form is the only legitimate
// way to mean "off").
func TestLoadVolumeCadenceRejectsNonPositive(t *testing.T) {
	cases := []struct{ name, field, value string }{
		{"sync_zero", "sync_every", "0s"},
		{"sync_negative", "sync_every", "-5m"},
		{"index_zero", "index_every", "0"},
		{"index_negative", "index_every", "-1h"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeConfig(t, `
[volumes.pictures]
path = "/p"
`+c.field+` = "`+c.value+`"
`)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), c.field) || !strings.Contains(err.Error(), "positive") {
				t.Fatalf("error = %v, want %s positive-duration error", err, c.field)
			}
		})
	}
}

// TestLoadVolumeCadenceRejectsBelowFloor catches the unit-typo class —
// `5` (no suffix) parses to 5ns, which the floor (>= 1s) rejects.
func TestLoadVolumeCadenceRejectsBelowFloor(t *testing.T) {
	p := writeConfig(t, `
[volumes.pictures]
path       = "/p"
sync_every = "500ms"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "at least 1s") {
		t.Fatalf("error = %v, want floor error", err)
	}
}

// TestLoadVolumeCadenceRejectsUnparseable: any string time.ParseDuration
// can't decode must surface as a load-time error rather than being
// silently dropped to zero.
func TestLoadVolumeCadenceRejectsUnparseable(t *testing.T) {
	cases := []struct{ name, field, value string }{
		{"sync_garbage", "sync_every", "tomorrow"},
		{"index_no_unit", "index_every", "fifteen-minutes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := writeConfig(t, `
[volumes.pictures]
path = "/p"
`+c.field+` = "`+c.value+`"
`)
			_, err := Load(p)
			if err == nil || !strings.Contains(err.Error(), c.field) {
				t.Fatalf("error = %v, want %s parse error", err, c.field)
			}
		})
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

// TestRemoteRoot pins the bucket-aware remote path root: rclone
// addresses bucket backends by leading path segment, so RemoteRoot must
// compose bucket+root for s3/b2/gcs and leave every other type's root
// untouched (an sftp root is legitimately absolute).
func TestRemoteRoot(t *testing.T) {
	t.Setenv("K", "k")
	body := `
[destinations.arch]
type              = "s3"
provider          = "Other"
bucket            = "bkt"
root              = "/"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }

[destinations.sub]
type              = "s3"
provider          = "Other"
bucket            = "bkt"
root              = "/deep/er"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }

[destinations.box]
type     = "sftp"
host     = "h"
user     = "u"
root     = "/abs/path"
password = { env = "K" }
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cases := map[string]string{
		"arch": "bkt",
		"sub":  "bkt/deep/er",
		"box":  "/abs/path",
	}
	for name, want := range cases {
		if got := cfg.Destinations[name].RemoteRoot(); got != want {
			t.Errorf("RemoteRoot(%s) = %q, want %q", name, got, want)
		}
	}
}

// TestRcloneSectionCryptBucketBackend pins that a crypt overlay on a
// bucket backend bakes the bucket into its remote line. Without it the
// overlay's transfers land in a bucket named after root's first
// segment (or the volume), invisible to the direct S3 reader that
// honours the configured bucket.
func TestRcloneSectionCryptBucketBackend(t *testing.T) {
	t.Setenv("K", "k")
	body := `
[destinations.arch]
type              = "s3"
provider          = "Other"
bucket            = "bkt"
root              = "/"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }

[destinations.arch.crypt]
password = "obscured"
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	section := cfg.Destinations["arch"].RcloneSection()
	if !strings.Contains(section, "remote = arch:bkt\n") {
		t.Fatalf("crypt remote must include the bucket, got:\n%s", section)
	}
}

// TestLoadDestinationVerifyEvery parses a per-destination verify_every on a
// content-addressed / packed destination.
func TestLoadDestinationVerifyEvery(t *testing.T) {
	t.Setenv("K", "v")
	p := writeConfig(t, `
[destinations.arch]
type         = "s3"
provider     = "AWS"
bucket       = "bkt"
root         = "/"
layout       = "packed"
verify_every = "168h"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Destinations["arch"].VerifyEvery != 168*time.Hour {
		t.Fatalf("VerifyEvery = %s, want 168h", cfg.Destinations["arch"].VerifyEvery)
	}
}

// TestLoadRejectsVerifyEveryOnMirror rejects verify_every on a layout that
// keeps no per-object fingerprints — verify has nothing to re-check there.
func TestLoadRejectsVerifyEveryOnMirror(t *testing.T) {
	p := writeConfig(t, `
[destinations.usb]
type         = "local"
root         = "/media/usb"
verify_every = "168h"
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "verify_every requires") {
		t.Fatalf("expected verify_every-layout error, got %v", err)
	}
}

// TestLoadRejectsBadVerifyEvery: an unparseable duration is caught at load,
// like the other cadence knobs.
func TestLoadRejectsBadVerifyEvery(t *testing.T) {
	t.Setenv("K", "v")
	p := writeConfig(t, `
[destinations.arch]
type         = "s3"
provider     = "AWS"
bucket       = "bkt"
root         = "/"
layout       = "content-addressed"
verify_every = "nope"
access_key_id     = { env = "K" }
secret_access_key = { env = "K" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "verify_every") {
		t.Fatalf("expected verify_every parse error, got %v", err)
	}
}

// TestLoadAgentVerifyEvery parses the fleet-wide default verify cadence.
func TestLoadAgentVerifyEvery(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen       = "127.0.0.1:9000"
auth         = { token = "t" }
verify_every = "336h"
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.VerifyEvery != 336*time.Hour {
		t.Fatalf("Agent.VerifyEvery = %s, want 336h", cfg.Agent.VerifyEvery)
	}
}

// TestLoadAgentVerifyEveryDefaultsZero: absent key leaves the default off.
func TestLoadAgentVerifyEveryDefaultsZero(t *testing.T) {
	p := writeConfig(t, `
[agent]
listen = "127.0.0.1:9000"
auth   = { token = "t" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agent.VerifyEvery != 0 {
		t.Fatalf("default Agent.VerifyEvery = %s, want 0", cfg.Agent.VerifyEvery)
	}
}

// TestLoadNodePullDurabilityEvery parses the per-node pull cadence — the
// receive-only htpc's clock for keeping its gate evidence fresh.
func TestLoadNodePullDurabilityEvery(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint              = "https://nas.local:8443"
path                  = "/mnt/nas-export"
pull_durability_every = "24h"
auth                  = { bearer = "b" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Nodes["nas"].PullDurabilityEvery != 24*time.Hour {
		t.Fatalf("PullDurabilityEvery = %s, want 24h", cfg.Nodes["nas"].PullDurabilityEvery)
	}
}

// TestLoadNodePullDurabilityEveryDefaultsZero: absent key leaves it off.
func TestLoadNodePullDurabilityEveryDefaultsZero(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint = "https://nas.local:8443"
path     = "/mnt/nas-export"
auth     = { bearer = "b" }
`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Nodes["nas"].PullDurabilityEvery != 0 {
		t.Fatalf("default PullDurabilityEvery = %s, want 0", cfg.Nodes["nas"].PullDurabilityEvery)
	}
}

// TestLoadRejectsBadPullDurabilityEvery: an unparseable duration is caught.
func TestLoadRejectsBadPullDurabilityEvery(t *testing.T) {
	p := writeConfig(t, `
[nodes.nas]
endpoint              = "https://nas.local:8443"
path                  = "/mnt/nas-export"
pull_durability_every = "soon"
auth                  = { bearer = "b" }
`)
	_, err := Load(p)
	if err == nil || !strings.Contains(err.Error(), "pull_durability_every") {
		t.Fatalf("expected pull_durability_every error, got %v", err)
	}
}
