package config

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"os/exec"
	"strings"
	"testing"
)

// revealRclone is the inverse of obscureRclone, implemented in the test to
// prove the obscure output round-trips (and matches rclone's format: IV in
// the first block, AES-CTR under the shared key).
func revealRclone(t *testing.T, obscured string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw) < aes.BlockSize {
		t.Fatalf("obscured value too short: %d bytes", len(raw))
	}
	block, err := aes.NewCipher(rcloneObscureCipher)
	if err != nil {
		t.Fatalf("cipher: %v", err)
	}
	iv := raw[:aes.BlockSize]
	out := make([]byte, len(raw)-aes.BlockSize)
	cipher.NewCTR(block, iv).XORKeyStream(out, raw[aes.BlockSize:])
	return string(out)
}

func TestObscureRcloneRoundTrips(t *testing.T) {
	for _, plaintext := range []string{"", "hunter2", "a much longer passphrase with spaces"} {
		got, err := obscureRclone(plaintext)
		if err != nil {
			t.Fatalf("obscureRclone(%q): %v", plaintext, err)
		}
		if got == plaintext && plaintext != "" {
			t.Fatalf("obscureRclone(%q) returned the plaintext unchanged", plaintext)
		}
		if revealed := revealRclone(t, got); revealed != plaintext {
			t.Fatalf("round trip: reveal(%q) = %q, want %q", got, revealed, plaintext)
		}
	}
}

// TestObscureRcloneDeterministic pins the property WriteRcloneConfig relies
// on: the same plaintext always obscures to the same bytes, so a re-render
// is byte-identical and the "unexpected rewrite is a signal" invariant
// holds. Two different plaintexts must not share an IV (keystream reuse).
func TestObscureRcloneDeterministic(t *testing.T) {
	a1, err := obscureRclone("secret")
	if err != nil {
		t.Fatalf("obscureRclone(secret): %v", err)
	}
	a2, err := obscureRclone("secret")
	if err != nil {
		t.Fatalf("obscureRclone(secret): %v", err)
	}
	if a1 != a2 {
		t.Fatalf("obscureRclone is not deterministic: %q vs %q", a1, a2)
	}
	b, err := obscureRclone("other")
	if err != nil {
		t.Fatalf("obscureRclone(other): %v", err)
	}
	// Compare the decoded leading IV bytes, not the base64 text: a base64
	// prefix comparison could mask a real IV collision or trip on unrelated
	// encoding differences.
	if bytes.Equal(obscuredIV(t, a1), obscuredIV(t, b)) {
		t.Fatalf("distinct plaintexts share an IV (keystream reuse): %q / %q", a1, b)
	}
}

// obscuredIV decodes an obscured value and returns its leading IV block.
func obscuredIV(t *testing.T, obscured string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(obscured)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw) < aes.BlockSize {
		t.Fatalf("obscured value too short for an IV: %d bytes", len(raw))
	}
	return raw[:aes.BlockSize]
}

// TestObscureRcloneMatchesRclone cross-checks the output against the real
// `rclone reveal`, proving squirrel speaks rclone's dialect. Skipped when
// rclone is not installed.
func TestObscureRcloneMatchesRclone(t *testing.T) {
	bin, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone not on PATH")
	}
	const plaintext = "correct horse battery staple"
	obscured, err := obscureRclone(plaintext)
	if err != nil {
		t.Fatalf("obscureRclone: %v", err)
	}
	out, err := exec.Command(bin, "reveal", obscured).Output()
	if err != nil {
		t.Fatalf("rclone reveal: %v", err)
	}
	if got := strings.TrimRight(string(out), "\n"); got != plaintext {
		t.Fatalf("rclone reveal = %q, want %q", got, plaintext)
	}
}
