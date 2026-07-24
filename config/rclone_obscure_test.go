package config

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"strings"
	"testing"
)

// reveal reverses rcloneObscure with the same fixed key — the Go equivalent
// of rclone's obscure.Reveal — so the round-trip can be asserted without a
// real rclone binary. It reads the IV from the ciphertext prefix, exactly as
// rclone does, which is why a fixed IV on the obscure side is harmless.
func reveal(t *testing.T, obscured string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		t.Fatalf("reveal: base64 decode %q: %v", obscured, err)
	}
	if len(raw) < aes.BlockSize {
		t.Fatalf("reveal: ciphertext too short (%d bytes)", len(raw))
	}
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		t.Fatalf("reveal: new cipher: %v", err)
	}
	iv, buf := raw[:aes.BlockSize], raw[aes.BlockSize:]
	cipher.NewCTR(block, iv).XORKeyStream(buf, buf)
	return string(buf)
}

// TestRcloneObscureRoundTrip is the core correctness pin: whatever squirrel
// obscures, rclone's reveal algorithm recovers unchanged, so a real rclone
// reads back exactly the plaintext squirrel resolved from config/env.
func TestRcloneObscureRoundTrip(t *testing.T) {
	for _, plaintext := range []string{
		"", "p", "hunter2", "transport-pw",
		"a longer password with spaces & symbols: /+=",
		"ünïcödé-π", strings.Repeat("x", 300),
	} {
		if got := reveal(t, rcloneObscure(plaintext)); got != plaintext {
			t.Errorf("reveal(obscure(%q)) = %q, want %q", plaintext, got, plaintext)
		}
	}
}

// TestRcloneObscureDeterministic pins the fixed-IV design: identical input
// obscures to identical output. WriteRcloneConfig relies on this — a random
// IV (rclone's own default) would rewrite rclone.conf on every render and
// destroy its "a rewrite is a signal" contract.
func TestRcloneObscureDeterministic(t *testing.T) {
	if a, b := rcloneObscure("transport-pw"), rcloneObscure("transport-pw"); a != b {
		t.Fatalf("obscure not deterministic: %q vs %q", a, b)
	}
}

// TestRcloneObscureGolden pins the exact bytes for a known input. It guards
// against an accidental edit to the key or the algorithm: any such change
// alters this output (and would silently break interop with real rclone).
func TestRcloneObscureGolden(t *testing.T) {
	const want = "AAAAAAAAAAAAAAAAAAAAACyuxxYwjZo"
	if got := rcloneObscure("hunter2"); got != want {
		t.Fatalf("rcloneObscure(\"hunter2\") = %q, want %q", got, want)
	}
}

// rcloneOptionNames lists, per rclone-backed destination type, the option
// names rclone's backend actually recognises that squirrel renders into
// rclone.conf. Each set was verified against rclone's backend source (the
// fs.Option `Name` values in backend/{sftp,s3,b2,gcs}). `bucket` is
// deliberately absent: it is squirrel's own path-composition key (like
// `root`) — rclone addresses a bucket as the leading path segment
// (Destination.RemoteRoot), not as a backend option, and ignores a `bucket`
// key inside the section. See squirrelPathKeys.
var rcloneOptionNames = map[string]map[string]bool{
	"sftp": {
		"host": true, "user": true, "port": true, "key_file": true,
		"known_hosts_file": true, "host_key_algorithms": true, "pass": true,
		"blake3sum_command": true, "hashes": true,
	},
	"s3": {
		"provider": true, "region": true, "endpoint": true, "storage_class": true,
		"access_key_id": true, "secret_access_key": true,
	},
	"b2":  {"account": true, "key": true},
	"gcs": {"service_account_file": true, "service_account_credentials": true},
}

// squirrelPathKeys are keys squirrel renders that rclone does not treat as
// backend options — it composes them into the remote path instead (see
// Destination.RemoteRoot). Only `bucket` qualifies today.
var squirrelPathKeys = map[string]bool{"bucket": true}

// TestRcloneSectionKeysMatchRcloneSchema is the regression guard for the F5
// bug class: squirrel rendering a credential under a name rclone doesn't
// recognise (sftp wanted `pass` not `password`; b2 `account`/`key` not
// `account_id`/`application_key`), which rclone silently ignores, failing
// the transfer in a baffling way. It pins every rendered key against the
// backend's real option names, and — via the guard below — forces any newly
// added rclone-backed type to enumerate its own, so the class can't recur.
func TestRcloneSectionKeysMatchRcloneSchema(t *testing.T) {
	t.Setenv("PW", "pw")
	t.Setenv("AK", "ak")
	t.Setenv("SK", "sk")
	t.Setenv("ACC", "acc")
	t.Setenv("APPKEY", "appkey")
	t.Setenv("SAC", "creds")
	cfg, err := Load(writeConfig(t, `
[destinations.box]
type                = "sftp"
host                = "h"
user                = "u"
root                = "/r"
port                = "2222"
key_file            = "/k"
known_hosts_file    = "/kh"
host_key_algorithms = "ssh-ed25519"
password            = { env = "PW" }
layout              = "content-addressed"

[destinations.s3d]
type              = "s3"
provider          = "AWS"
bucket            = "bkt"
root              = "/r"
region            = "eu"
endpoint          = "https://e"
storage_class     = "STANDARD"
access_key_id     = { env = "AK" }
secret_access_key = { env = "SK" }

[destinations.b2d]
type            = "b2"
bucket          = "bkt"
root            = "/r"
account_id      = { env = "ACC" }
application_key = { env = "APPKEY" }

[destinations.gcsd]
type                        = "gcs"
bucket                      = "bkt"
root                        = "/r"
service_account_file        = "/sa"
service_account_credentials = { env = "SAC" }
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// A new rclone-backed type that forgets to pin its option names fails
	// here — the author must enumerate rclone's real names to satisfy it.
	for typ, schema := range destSchemas {
		if schema.rcloneType == "" {
			continue
		}
		if _, ok := rcloneOptionNames[typ]; !ok {
			t.Errorf("type %q is rclone-backed but has no rcloneOptionNames entry — add its real rclone option names", typ)
		}
	}

	fixtures := map[string]string{"sftp": "box", "s3": "s3d", "b2": "b2d", "gcs": "gcsd"}
	for typ, allowed := range rcloneOptionNames {
		t.Run(typ, func(t *testing.T) {
			name := fixtures[typ]
			if name == "" {
				t.Fatalf("no fixture destination for type %q", typ)
			}
			for _, key := range renderedKeys(t, cfg.Destinations[name].RcloneSection()) {
				if allowed[key] || squirrelPathKeys[key] {
					continue
				}
				t.Errorf("type %q renders key %q, not a recognised rclone %s option — rclone would ignore it (see rcloneOptionNames)", typ, key, typ)
			}
		})
	}
}

// TestRcloneSectionSFTPPassObscured pins that sftp renders rclone's `pass`
// option (never the ignored `password`), obscured, and revealing back to the
// plaintext squirrel held — the transfer-time contract behind F5.
func TestRcloneSectionSFTPPassObscured(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.nas]
type     = "sftp"
host     = "h"
user     = "u"
root     = "/r"
password = "s3cr3t"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	section := cfg.Destinations["nas"].RcloneSection()
	if strings.Contains(section, "password") {
		t.Fatalf("sftp section uses the ignored `password` key:\n%s", section)
	}
	if strings.Contains(section, "s3cr3t") {
		t.Fatalf("plaintext sftp password leaked into the section:\n%s", section)
	}
	got, ok := passLine(section)
	if !ok {
		t.Fatalf("sftp section has no `pass` line:\n%s", section)
	}
	if rev := reveal(t, got); rev != "s3cr3t" {
		t.Fatalf("pass value reveals to %q, want %q", rev, "s3cr3t")
	}
}

// passLine returns the value of the `pass = ...` line in an rclone.conf
// section, if present.
func passLine(section string) (string, bool) {
	for _, line := range strings.Split(section, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "pass = "); ok {
			return v, true
		}
	}
	return "", false
}

// renderedKeys returns the option keys of an rclone.conf section body,
// skipping the `[section]` headers and the `type` line.
func renderedKeys(t *testing.T, section string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "[") {
			continue
		}
		k, _, ok := strings.Cut(line, " = ")
		if !ok {
			t.Fatalf("malformed rclone.conf line %q in:\n%s", line, section)
		}
		if k != "type" {
			keys = append(keys, k)
		}
	}
	return keys
}
