package config

import (
	"strings"
	"testing"
)

// TestLoadHashAlgoDefaultsForContentAddressedSFTP: a content-addressed
// sftp destination defaults to sha256 fingerprints (rendered as the sftp
// `hashes` option) while a mirrored one keeps rclone's own hash
// behaviour untouched.
func TestLoadHashAlgoDefaultsForContentAddressedSFTP(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.archive]
type   = "sftp"
host   = "host.example"
user   = "u"
root   = "/data"
layout = "content-addressed"

[destinations.mirror]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	archive := cfg.Destinations["archive"]
	if archive.HashAlgo != "sha256" {
		t.Fatalf("HashAlgo = %q, want default sha256 for content-addressed sftp", archive.HashAlgo)
	}
	if !strings.Contains(archive.RcloneSection(), "hashes = sha256\n") {
		t.Fatalf("section lacks hashes line:\n%s", archive.RcloneSection())
	}
	mirror := cfg.Destinations["mirror"]
	if mirror.HashAlgo != "" {
		t.Fatalf("HashAlgo = %q, want empty for mirrored sftp", mirror.HashAlgo)
	}
	if strings.Contains(mirror.RcloneSection(), "hashes") {
		t.Fatalf("mirrored section unexpectedly renders hashes:\n%s", mirror.RcloneSection())
	}
}

// TestLoadHashAlgoExplicit: an explicit hash_algo overrides the default
// and renders on mirrored sftp destinations too.
func TestLoadHashAlgoExplicit(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.archive]
type      = "sftp"
host      = "host.example"
user      = "u"
root      = "/data"
layout    = "content-addressed"
hash_algo = "md5"

[destinations.mirror]
type      = "sftp"
host      = "host.example"
user      = "u"
root      = "/data"
hash_algo = "sha256"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["archive"].HashAlgo; got != "md5" {
		t.Fatalf("HashAlgo = %q, want md5", got)
	}
	if got := cfg.Destinations["mirror"].HashAlgo; got != "sha256" {
		t.Fatalf("HashAlgo = %q, want sha256", got)
	}
	if !strings.Contains(cfg.Destinations["mirror"].RcloneSection(), "hashes = sha256\n") {
		t.Fatalf("mirror section lacks hashes line:\n%s", cfg.Destinations["mirror"].RcloneSection())
	}
}

func TestLoadRejectsHashAlgoOnNonSFTP(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.bucket]
type      = "s3"
provider  = "AWS"
bucket    = "b"
root      = "p"
hash_algo = "sha256"
`))
	if err == nil || !strings.Contains(err.Error(), "hash_algo") {
		t.Fatalf("err = %v, want hash_algo rejection on s3", err)
	}
}

func TestLoadRejectsUnknownHashAlgo(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.archive]
type      = "sftp"
host      = "host.example"
user      = "u"
root      = "/data"
hash_algo = "sha512"
`))
	if err == nil || !strings.Contains(err.Error(), "hash_algo") {
		t.Fatalf("err = %v, want unknown hash_algo rejection", err)
	}
}

func TestLoadCheckers(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.archive]
type     = "sftp"
host     = "host.example"
user     = "u"
root     = "/data"
checkers = 4
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["archive"]
	if d.Checkers != 4 {
		t.Fatalf("Checkers = %d, want 4", d.Checkers)
	}
	if strings.Contains(d.RcloneSection(), "checkers") {
		t.Fatalf("checkers leaked into rclone.conf (it is an invocation flag):\n%s", d.RcloneSection())
	}
}

func TestLoadRejectsBadCheckers(t *testing.T) {
	cases := []struct{ name, body string }{
		{"zero", "checkers = 0"},
		{"negative", "checkers = -2"},
		{"string", `checkers = "4"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, `
[destinations.archive]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
`+c.body+"\n"))
			if err == nil || !strings.Contains(err.Error(), "checkers") {
				t.Fatalf("err = %v, want checkers rejection", err)
			}
		})
	}
}

func TestLoadRejectsCheckersOnKopia(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.repo]
type     = "kopia"
root     = "/repo"
password = "pw"
checkers = 4
`))
	if err == nil || !strings.Contains(err.Error(), "checkers") {
		t.Fatalf("err = %v, want checkers rejection on kopia", err)
	}
}
