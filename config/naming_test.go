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

// TestDeriveNamingKeyGolden pins the derivation against a fixed vector.
// The key names every artifact an encrypted archive destination stores, so
// a change here renames the whole root and orphans everything already
// uploaded — this test exists to make that break loud and deliberate rather
// than a silent incident on someone's next sync.
func TestDeriveNamingKeyGolden(t *testing.T) {
	both := DeriveNamingKey("hunter2", "the-salt")
	got := hex.EncodeToString(both[:])
	const want = "8b0b263faa7f2fa99f574e75c89c9e6fdb862fe08b442832dda9b745ce91e1e9"
	if got != want {
		t.Errorf("DeriveNamingKey(hunter2, the-salt) = %s, want %s", got, want)
	}
	noSalt := DeriveNamingKey("hunter2", "")
	gotNoSalt := hex.EncodeToString(noSalt[:])
	const wantNoSalt = "6f4f9d34e8a3ffa33c67aa739cb910e1edba2fb968a3b82c296b4919d6b3cae3"
	if gotNoSalt != wantNoSalt {
		t.Errorf("DeriveNamingKey(hunter2, \"\") = %s, want %s", gotNoSalt, wantNoSalt)
	}
	if NamingKeyContext != "squirrel destination artifact naming v1" {
		t.Errorf("NamingKeyContext = %q; changing it renames every stored artifact", NamingKeyContext)
	}
}

// TestDeriveNamingKeySeparatesFields covers the NUL separator: two password
// pairs that would concatenate to the same string must not share a key.
func TestDeriveNamingKeySeparatesFields(t *testing.T) {
	if DeriveNamingKey("ab", "c") == DeriveNamingKey("a", "bc") {
		t.Fatal("password fields run together: (ab,c) and (a,bc) derive one key")
	}
	if DeriveNamingKey("hunter2", "") == DeriveNamingKey("hunter2", "x") {
		t.Fatal("adding a salt left the naming key unchanged")
	}
}

// TestNamingKeyIdenticalAcrossConfigForms is the migration guarantee: the
// same password reaches the same key whether the config supplied plaintext
// or a pre-obscured value. Squirrel obscures with a fixed zero
// initialisation vector while `rclone obscure` draws a random one, so the
// two stored forms differ byte-for-byte; deriving from the revealed
// plaintext is what keeps them naming one archive.
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
// and are held to a revealable password; a crypt mirror never derives one,
// so it keeps passing whatever pre-obscured value it always did straight
// through to rclone.
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
// layout refuses it at load — naming the field — rather than deriving from
// a value rclone would reject later anyway. A mirror is unaffected.
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
// surfaces as an error rather than decoding to noise.
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
