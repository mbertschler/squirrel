package config

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// rcloneObscureCipher is the fixed AES-256 key rclone uses to obscure
// credentials in its config file. It is copied verbatim from rclone's
// fs/config/obscure package so squirrel can produce the exact same
// representation rclone's own `rclone obscure` emits and `rclone reveal`
// (and every rclone backend) decodes.
//
// This is obfuscation, not encryption: the key is public (baked into
// rclone's open source and reproduced here), so an obscured value protects
// only against casual snooping of the rendered rclone.conf, never against
// an attacker. squirrel keeps rclone.conf at mode 0600 for the real
// protection; obscuring exists solely to speak rclone's config dialect.
var rcloneObscureCipher = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// obscureIVSalt namespaces the deterministic IV derivation below so the
// derived bytes can never coincide with another squirrel hash that also
// feeds plaintext into SHA-256.
const obscureIVSalt = "squirrel-rclone-obscure\x00"

// obscureRclone returns the rclone-obscured form of a plaintext secret —
// AES-CTR under the public rcloneObscureCipher with the 16-byte IV
// prepended, base64-url encoded, exactly rclone's wire format.
//
// Unlike rclone's own obscure (which draws a random IV), squirrel derives
// the IV deterministically from the plaintext, so a given secret always
// obscures to the same bytes. That determinism is load-bearing: the
// rendered rclone.conf is compared against the file on disk and only
// rewritten when it changes (sync.WriteRcloneConfig), and an unexpected
// rewrite is a signal that a resolver regressed a credential — a random IV
// would rewrite the file on every run and destroy that signal. A
// deterministic IV loses nothing security-wise because obscure is not
// security in the first place (the key is public); deriving it per-plaintext
// (rather than a constant) still avoids reusing an AES-CTR keystream across
// two different secrets.
func obscureRclone(plaintext string) (string, error) {
	block, err := aes.NewCipher(rcloneObscureCipher)
	if err != nil {
		return "", fmt.Errorf("obscure: init cipher: %w", err)
	}
	buf := make([]byte, aes.BlockSize+len(plaintext))
	iv := buf[:aes.BlockSize]
	digest := sha256.Sum256([]byte(obscureIVSalt + plaintext))
	copy(iv, digest[:aes.BlockSize])
	stream := cipher.NewCTR(block, iv)
	stream.XORKeyStream(buf[aes.BlockSize:], []byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
