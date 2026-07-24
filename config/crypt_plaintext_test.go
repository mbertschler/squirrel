package config

import (
	"strings"
	"testing"
)

// TestLoadCryptObscuresPlaintext is the default path (F2): a crypt password
// is supplied in plaintext and squirrel obscures it into rclone's form at
// load time, so the rendered value is not the plaintext and reveals back to
// it.
func TestLoadCryptObscuresPlaintext(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/data"

[destinations.offsite.crypt]
password  = "hunter2"
password2 = "the-salt"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Destinations["offsite"].Crypt
	if c.Password == "hunter2" {
		t.Fatalf("crypt password was stored plaintext, not obscured")
	}
	if got := reveal(t, c.Password); got != "hunter2" {
		t.Fatalf("obscured password reveals to %q, want %q", got, "hunter2")
	}
	if got := reveal(t, c.Password2); got != "the-salt" {
		t.Fatalf("obscured salt reveals to %q, want %q", got, "the-salt")
	}
}

// TestLoadCryptObscuredMarkerKeepsVerbatim covers the migration path: with
// `obscured = true` the pre-obscured values are stored (and rendered)
// verbatim.
func TestLoadCryptObscuredMarkerKeepsVerbatim(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/data"

[destinations.offsite.crypt]
obscured  = true
password  = "already-obscured"
password2 = "already-salt"
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	c := cfg.Destinations["offsite"].Crypt
	if c.Password != "already-obscured" || c.Password2 != "already-salt" {
		t.Fatalf("obscured marker did not keep values verbatim: %+v", c)
	}
}

func TestLoadCryptObscuredMustBeBool(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.offsite]
type = "sftp"
host = "h"
user = "u"
root = "/data"

[destinations.offsite.crypt]
obscured = "yes"
password = "hunter2"
`))
	if err == nil || !strings.Contains(err.Error(), "crypt: obscured must be a boolean") {
		t.Fatalf("expected obscured-must-be-bool error, got %v", err)
	}
}
