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
// and renders on an encrypted sftp mirror, which rclone still writes.
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

[destinations.mirror.crypt]
password = "pw"
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

// TestNativeContentLayoutsTakeTheirOwnHashes: a content-addressed or packed
// sftp destination squirrel writes itself defaults to sha256, accepts the
// hashes whose command squirrel runs and checks, refuses the ones only
// rclone reads, and refuses checkers; one behind crypt keeps rclone's.
func TestNativeContentLayoutsTakeTheirOwnHashes(t *testing.T) {
	dest := func(extra string) string {
		return "[destinations.archive]\ntype = \"sftp\"\nhost = \"h\"\nuser = \"u\"\nroot = \"/data\"\nlayout = \"packed\"\n" + extra
	}
	cfg, err := Load(writeConfig(t, dest("")))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["archive"].HashAlgo; got != "sha256" {
		t.Fatalf("HashAlgo = %q, want the sha256 default on a native packed destination", got)
	}
	if _, err := Load(writeConfig(t, dest(`hash_algo = "blake3"`+"\n"))); err != nil {
		t.Fatalf("blake3 on a native packed destination: %v", err)
	}
	for _, key := range []string{`hash_algo = "xxh3"`, "checkers = 4"} {
		name, _, _ := strings.Cut(key, " ")
		if _, err := Load(writeConfig(t, dest(key+"\n"))); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("%s on a native packed destination: err = %v, want a rejection naming it", key, err)
		}
	}
	if _, err := Load(writeConfig(t, dest(`hash_algo = "xxh3"`+"\n\n[destinations.archive.crypt]\npassword = \"pw\"\n"))); err != nil {
		t.Fatalf("xxh3 behind crypt, which rclone writes: %v", err)
	}
}

// TestLoadRejectsRcloneKeysOnNativeMirrors: squirrel writes a plain sftp
// mirror itself, so the keys that only tune rclone are refused there.
func TestLoadRejectsRcloneKeysOnNativeMirrors(t *testing.T) {
	for _, key := range []string{`hash_algo = "sha256"`, "checkers = 4"} {
		_, err := Load(writeConfig(t, `
[destinations.mirror]
type = "sftp"
host = "host.example"
user = "u"
root = "/data"
`+key+"\n"))
		name, _, _ := strings.Cut(key, " ")
		if err == nil || !strings.Contains(err.Error(), name) || !strings.Contains(err.Error(), "squirrel") {
			t.Fatalf("%s on a plain sftp mirror: err = %v, want a rejection naming the key", name, err)
		}
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
layout   = "content-addressed"
checkers = 4

[destinations.archive.crypt]
password = "pw"
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
type   = "sftp"
host   = "host.example"
user   = "u"
root   = "/data"
layout = "content-addressed"
`+c.body+"\n"))
			if err == nil || !strings.Contains(err.Error(), "checkers") {
				t.Fatalf("err = %v, want checkers rejection", err)
			}
		})
	}
}

// TestLoadConcurrency: every destination but kopia accepts a positive
// concurrency, native and rclone-written alike, and it stays out of
// rclone.conf.
func TestLoadConcurrency(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.usb]
type        = "local"
root        = "/mnt/usb"
concurrency = 2

[destinations.archive]
type        = "sftp"
host        = "host.example"
user        = "u"
root        = "/data"
concurrency = 16

[destinations.archive.crypt]
password = "pw"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Destinations["usb"].Concurrency; got != 2 {
		t.Fatalf("usb Concurrency = %d, want 2", got)
	}
	archive := cfg.Destinations["archive"]
	if archive.Concurrency != 16 {
		t.Fatalf("archive Concurrency = %d, want 16", archive.Concurrency)
	}
	if strings.Contains(archive.RcloneSection(), "concurrency") {
		t.Fatalf("concurrency leaked into rclone.conf (it is an invocation flag):\n%s", archive.RcloneSection())
	}
}

func TestLoadRejectsBadConcurrency(t *testing.T) {
	cases := []struct{ name, body string }{
		{"zero", "type = \"local\"\nroot = \"/mnt/usb\"\nconcurrency = 0"},
		{"negative", "type = \"local\"\nroot = \"/mnt/usb\"\nconcurrency = -1"},
		{"string", "type = \"local\"\nroot = \"/mnt/usb\"\nconcurrency = \"4\""},
		{"kopia", "type = \"kopia\"\nroot = \"/repo\"\npassword = \"pw\"\nconcurrency = 4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "[destinations.d]\n"+c.body+"\n"))
			if err == nil || !strings.Contains(err.Error(), "concurrency") {
				t.Fatalf("err = %v, want concurrency rejection", err)
			}
		})
	}
}

func TestLoadForcePathStyle(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.bucket]
type             = "s3"
provider         = "Minio"
bucket           = "b"
root             = "p"
endpoint         = "https://minio.local:9000"
layout           = "content-addressed"
force_path_style = true
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := cfg.Destinations["bucket"]
	if !d.PathStyle {
		t.Fatalf("PathStyle = %v, want true", d.PathStyle)
	}
	// The knob governs squirrel's own S3 client, not the rclone transport,
	// so it must not leak into rclone.conf.
	if strings.Contains(d.RcloneSection(), "force_path_style") {
		t.Fatalf("force_path_style leaked into rclone.conf:\n%s", d.RcloneSection())
	}
}

func TestLoadForcePathStyleDefaultsOff(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.bucket]
type     = "s3"
provider = "AWS"
bucket   = "b"
root     = "p"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Destinations["bucket"].PathStyle {
		t.Fatalf("PathStyle defaulted on")
	}
}

func TestLoadRejectsForcePathStyleOnNonS3(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.archive]
type             = "sftp"
host             = "host.example"
user             = "u"
root             = "/data"
force_path_style = true
`))
	if err == nil || !strings.Contains(err.Error(), "force_path_style") {
		t.Fatalf("err = %v, want force_path_style rejection on sftp", err)
	}
}

func TestLoadRejectsNonBoolForcePathStyle(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.bucket]
type             = "s3"
provider         = "AWS"
bucket           = "b"
root             = "p"
force_path_style = "yes"
`))
	if err == nil || !strings.Contains(err.Error(), "force_path_style") {
		t.Fatalf("err = %v, want boolean rejection", err)
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
