package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// TestDeriveNamingKeyGolden pins the derivation against fixed vectors,
// cross-checked against Python's hashlib.scrypt and the blake3 package. The
// key names every artifact an encrypted archive destination stores, so a
// change here renames the whole root and orphans everything uploaded.
func TestDeriveNamingKeyGolden(t *testing.T) {
	for _, tc := range []struct {
		password, password2, want string
	}{
		{"hunter2", "the-salt", "770c6d1e97f8803b232236d7147f809d3a3fc19e129ee93d2d961d76c3ac996f"},
		{"hunter2", "", "2edefdfcc3ac0124cd86c83baffc0093e3dd01ae8cbe23e8e8c5f0d1f2e03935"},
	} {
		key := DeriveNamingKey(tc.password, tc.password2)
		if got := hex.EncodeToString(key[:]); got != tc.want {
			t.Errorf("DeriveNamingKey(%q, %q) = %s, want %s", tc.password, tc.password2, got, tc.want)
		}
	}
	if NamingKeyContext != "squirrel destination artifact naming v1" {
		t.Errorf("NamingKeyContext = %q; changing it renames every stored artifact", NamingKeyContext)
	}
}

// TestDeriveNamingKeySeparatesFields: the password and the salt are
// separate inputs, so two pairs that concatenate to one string derive two
// keys, and adding a salt changes the key.
func TestDeriveNamingKeySeparatesFields(t *testing.T) {
	if DeriveNamingKey("ab", "c") == DeriveNamingKey("a", "bc") {
		t.Fatal("password fields run together: (ab,c) and (a,bc) derive one key")
	}
	if DeriveNamingKey("hunter2", "") == DeriveNamingKey("hunter2", "x") {
		t.Fatal("adding a salt left the naming key unchanged")
	}
}

// TestNamingKeyIdenticalAcrossConfigForms: the same password reaches the
// same key whether the config supplied plaintext or a pre-obscured value.
// Squirrel obscures with a fixed zero initialisation vector while `rclone
// obscure` draws a random one, so the two stored forms differ
// byte-for-byte; deriving from the revealed plaintext is what keeps them
// naming one archive.
func TestNamingKeyIdenticalAcrossConfigForms(t *testing.T) {
	const password = "hunter2"
	plain, err := Load(writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "h"
user   = "u"
root   = "/data"
layout = "content-addressed"

[destinations.offsite.crypt]
password = "`+password+`"
`))
	if err != nil {
		t.Fatalf("Load plaintext form: %v", err)
	}
	preObscured, err := Load(writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "h"
user   = "u"
root   = "/data"
layout = "content-addressed"

[destinations.offsite.crypt]
obscured = true
password = "`+obscureWithRandomIV(t, password)+`"
`))
	if err != nil {
		t.Fatalf("Load pre-obscured form: %v", err)
	}
	a := plain.Destinations["offsite"].Crypt
	b := preObscured.Destinations["offsite"].Crypt
	if a.Password == b.Password {
		t.Fatal("the two stored forms are identical; this test no longer covers the random-IV case")
	}
	if a.NamingKey != b.NamingKey {
		t.Fatalf("naming keys differ across config forms:\n plaintext    %x\n pre-obscured %x", a.NamingKey, b.NamingKey)
	}
}

// TestNamingKeyDerivedOnlyWhereNeeded: the append-only layouts get a key
// and are held to a revealable password; a crypt mirror derives none and
// passes its pre-obscured value straight through to rclone.
func TestNamingKeyDerivedOnlyWhereNeeded(t *testing.T) {
	for _, tc := range []struct {
		layout    string
		wantKey   bool
		wantHides bool
	}{
		{"content-addressed", true, true},
		{"packed", true, true},
		{"mirror", false, false},
	} {
		t.Run(tc.layout, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "h"
user   = "u"
root   = "/data"
layout = "`+tc.layout+`"

[destinations.offsite.crypt]
password = "hunter2"
`))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			d := cfg.Destinations["offsite"]
			if got := d.Crypt.NamingKey != [32]byte{}; got != tc.wantKey {
				t.Errorf("derived a naming key = %v, want %v", got, tc.wantKey)
			}
			if got := d.HidesArtifactNames(); got != tc.wantHides {
				t.Errorf("HidesArtifactNames = %v, want %v", got, tc.wantHides)
			}
		})
	}
}

// TestUnrevealablePasswordRefusedForKeyedLayouts: an `obscured = true`
// value squirrel cannot reveal cannot name artifacts either, so an archive
// layout refuses it at load, naming the field. A mirror is unaffected.
func TestUnrevealablePasswordRefusedForKeyedLayouts(t *testing.T) {
	body := func(layout string) string {
		return `
[destinations.offsite]
type   = "sftp"
host   = "h"
user   = "u"
root   = "/data"
layout = "` + layout + `"

[destinations.offsite.crypt]
obscured = true
password = "not-really-obscured"
`
	}
	_, err := Load(writeConfig(t, body("content-addressed")))
	if err == nil || !strings.Contains(err.Error(), "crypt.password") {
		t.Fatalf("want a crypt.password refusal for a keyed layout, got %v", err)
	}
	if _, err := Load(writeConfig(t, body("mirror"))); err != nil {
		t.Fatalf("a crypt mirror must still accept a pre-obscured value verbatim: %v", err)
	}
}

// TestEmptyRevealedPasswordRefused: an `obscured = true` value that reveals
// to "" leaves rclone encrypting under an all-zero key, so it is refused at
// load rather than named as if it were a secret.
func TestEmptyRevealedPasswordRefused(t *testing.T) {
	_, err := Load(writeConfig(t, `
[destinations.offsite]
type   = "sftp"
host   = "h"
user   = "u"
root   = "/data"
layout = "packed"

[destinations.offsite.crypt]
obscured = true
password = "`+rcloneObscure("")+`"
`))
	if err == nil || !strings.Contains(err.Error(), "empty password") {
		t.Fatalf("want an empty-password refusal, got %v", err)
	}
}

// TestHidesArtifactNamesNeedsCrypt: an unencrypted archive destination has
// no key to derive from, so it names artifacts by content hash.
func TestHidesArtifactNamesNeedsCrypt(t *testing.T) {
	d := &Destination{Layout: LayoutContentAddressed}
	if d.HidesArtifactNames() {
		t.Fatal("an unencrypted content-addressed destination reported keyed names")
	}
}

// TestRcloneRevealMatchesIndependentReveal cross-checks rcloneReveal
// against the test file's own implementation, over both initialisation
// vectors it must accept: squirrel's fixed zero one and rclone's random one.
func TestRcloneRevealMatchesIndependentReveal(t *testing.T) {
	for _, secret := range []string{"", "hunter2", "a longer pass phrase with spaces"} {
		for name, obscured := range map[string]string{
			"zero-iv":   rcloneObscure(secret),
			"random-iv": obscureWithRandomIV(t, secret),
		} {
			got, err := rcloneReveal(obscured)
			if err != nil {
				t.Fatalf("rcloneReveal(%s of %q): %v", name, secret, err)
			}
			if got != secret {
				t.Errorf("rcloneReveal(%s of %q) = %q", name, secret, got)
			}
			if want := reveal(t, obscured); got != want {
				t.Errorf("rcloneReveal disagrees with the independent reveal: %q vs %q", got, want)
			}
		}
	}
}

// TestRcloneRevealRejectsMalformed: a value that is not obscured at all
// surfaces as an error.
func TestRcloneRevealRejectsMalformed(t *testing.T) {
	for _, in := range []string{"not base64 !!", "c2hvcnQ"} {
		if _, err := rcloneReveal(in); err == nil {
			t.Errorf("rcloneReveal(%q) = nil error, want a refusal", in)
		}
	}
}

// obscureWithRandomIV reproduces `rclone obscure`, which draws a random
// initialisation vector — the form a config written before squirrel
// obscured its own passwords carries.
func obscureWithRandomIV(t *testing.T, plaintext string) string {
	t.Helper()
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		t.Fatalf("new cipher: %v", err)
	}
	buf := make([]byte, aes.BlockSize+len(plaintext))
	if _, err := rand.Read(buf[:aes.BlockSize]); err != nil {
		t.Fatalf("read iv: %v", err)
	}
	cipher.NewCTR(block, buf[:aes.BlockSize]).XORKeyStream(buf[aes.BlockSize:], []byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(buf)
}
