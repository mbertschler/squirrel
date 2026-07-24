package config

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
)

// rcloneObscureKey is rclone's fixed, published AES-256 key for its
// reversible credential "obscuring" (rclone's fs/config/obscure). It grants
// no real secrecy — the key ships in rclone's source — it only keeps a
// plaintext credential from sitting in rclone.conf verbatim. rclone's
// backends that mark a secret "obscured" (sftp's `pass`, crypt's password)
// reject an un-obscured value, so squirrel, which holds the plaintext from
// its own config, must apply the identical transform when it renders
// rclone.conf. The bytes must match rclone's exactly or rclone cannot reveal
// the value; they are pinned against rclone's source by test.
var rcloneObscureKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// rcloneObscureCipher is the AES-256 block built once from the fixed key.
// aes.NewCipher only errors on an invalid key length, so a panic here can
// only mean rcloneObscureKey was edited to the wrong size — a programming
// error surfaced at package init rather than a runtime failure path, which
// is what lets rcloneObscure return a bare string.
var rcloneObscureCipher = func() cipher.Block {
	block, err := aes.NewCipher(rcloneObscureKey)
	if err != nil {
		panic("config: invalid rclone obscure key: " + err.Error())
	}
	return block
}()

// rcloneObscure reproduces rclone's obscure.Obscure: AES-CTR under the fixed
// published key, the IV prepended to the ciphertext, the whole encoded with
// base64 raw-URL. rclone's own `rclone obscure` draws a random IV; squirrel
// fixes it at zero deliberately. rclone.Reveal recovers the value from
// whatever IV the ciphertext carries, so a zero IV round-trips identically —
// and a deterministic render keeps rclone.conf byte-stable, so
// WriteRcloneConfig rewrites it only on a genuine credential change (its
// churn-is-a-signal contract) instead of every time the agent renders.
//
// The fixed IV is a deliberate trade-off, not free: in CTR mode a constant IV
// makes identical plaintexts obscure to identical ciphertext, so the output
// leaks equality between secrets (two destinations sharing one password render
// to the same string) — a leak rclone's random IV avoids. It is acceptable
// here only because (a) obscuring provides no real secrecy to begin with — the
// AES key is published in rclone's source, so a reader with the file can
// reveal any value regardless of IV — and (b) byte-stable output is what
// preserves WriteRcloneConfig's churn-is-a-signal contract. Do not carry this
// fixed-IV choice into any context that expects obscuring to hide anything.
//
// It lives here, unexported but reusable across the config package, because
// the crypt-config work also needs to obscure a password without shelling
// out to rclone.
func rcloneObscure(plaintext string) string {
	// buf[:aes.BlockSize] is the (zero) IV, left in place as the prefix;
	// the ciphertext lands after it.
	buf := make([]byte, aes.BlockSize+len(plaintext))
	cipher.NewCTR(rcloneObscureCipher, buf[:aes.BlockSize]).XORKeyStream(buf[aes.BlockSize:], []byte(plaintext))
	return base64.RawURLEncoding.EncodeToString(buf)
}
