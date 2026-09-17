package config

import "github.com/zeebo/blake3"

// NamingKeyContext is the BLAKE3 key-derivation context that binds a
// destination's artifact-naming key to this scheme and version. It is part
// of the on-remote format: change it and every name a destination derives
// changes with it, orphaning every artifact already stored under the old
// names. Recovery tooling that reproduces the names without squirrel must
// pass this exact string.
const NamingKeyContext = "squirrel destination artifact naming v1"

// DeriveNamingKey derives a destination's artifact-naming key from its
// plaintext crypt passwords. Deriving it — rather than configuring it —
// keeps the secret an operator must preserve exactly the pair they already
// must preserve to decrypt the data at all, and keeps the names
// reproducible from the config alone, so no key material has to survive
// alongside the archive for it to be recoverable.
//
// The passwords are joined by a NUL separator so that ("ab", "c") and
// ("a", "bc") derive different keys.
func DeriveNamingKey(password, password2 string) [32]byte {
	material := make([]byte, 0, len(password)+1+len(password2))
	material = append(material, password...)
	material = append(material, 0)
	material = append(material, password2...)
	var key [32]byte
	blake3.DeriveKey(NamingKeyContext, material, key[:])
	return key
}
