package config

import (
	"github.com/zeebo/blake3"
	"golang.org/x/crypto/scrypt"
)

// NamingKeyContext is the BLAKE3 key-derivation context that binds a
// destination's artifact-naming key to this scheme and version. It is part
// of the on-remote format: change it and every name a destination derives
// changes with it, orphaning every artifact already stored under the old
// names. Recovery tooling that reproduces the names without squirrel must
// pass this exact string.
const NamingKeyContext = "squirrel destination artifact naming v1"

// The scrypt parameters and default salt of rclone crypt's own key
// derivation (backend/crypt/cipher.go). Stretching the passwords exactly as
// rclone does makes a password guess tested against the stored names cost
// the same work as one tested against the encrypted data.
const (
	cryptScryptN      = 16384
	cryptScryptR      = 8
	cryptScryptP      = 1
	cryptScryptKeyLen = 80
)

var cryptDefaultSalt = []byte{0xA8, 0x0D, 0xF4, 0x3A, 0x8F, 0xBD, 0x03, 0x08, 0xA7, 0xCA, 0xB8, 0x3E, 0x58, 0x1F, 0x86, 0xB1}

// DeriveNamingKey derives a destination's artifact-naming key from its
// plaintext crypt passwords: rclone crypt's scrypt stretch of password,
// salted by password2 (or rclone's default salt when password2 is empty),
// then BLAKE3 key derivation under NamingKeyContext. The key is a function
// of the passwords alone, so the names stay reproducible from the config.
func DeriveNamingKey(password, password2 string) [32]byte {
	salt := cryptDefaultSalt
	if password2 != "" {
		salt = []byte(password2)
	}
	material, err := scrypt.Key([]byte(password), salt, cryptScryptN, cryptScryptR, cryptScryptP, cryptScryptKeyLen)
	if err != nil {
		panic("config: invalid scrypt parameters: " + err.Error())
	}
	var key [32]byte
	blake3.DeriveKey(NamingKeyContext, material, key[:])
	return key
}
