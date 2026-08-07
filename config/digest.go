package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/zeebo/blake3"
)

// DigestLen is the byte length of a config content digest. It is a
// BLAKE3-256 hash, the same function squirrel identifies file content
// with, so a config digest is stored in the same fixed-length BLOB shape
// as every other hash in the index.
const DigestLen = 32

// digestBytes is the single definition of "the content identity of a
// config file": the BLAKE3-256 of its exact bytes. Load hashes the bytes
// it decoded and FileDigest hashes a fresh read of the same file, so the
// agent's drift comparison can never be fooled by two different hashing
// rules.
//
// Content, deliberately, not (size, mtime): a rewrite that produces
// identical bytes — a `touch`, an editor saving an unmodified buffer, a
// configuration-management tool re-rendering the same template — leaves
// this digest unchanged and must not read as a change.
func digestBytes(data []byte) []byte {
	sum := blake3.Sum256(data)
	return sum[:]
}

// FileDigest returns the content digest of the config file at path without
// parsing or resolving it — the cheap half of Load, for the agent's
// periodic drift re-check (F9). It reads the file the same way Load does
// and hashes it with the same function, so a digest from here and a digest
// from Load are comparable by construction.
//
// A missing file yields a *MissingError (matchable with IsMissing), the
// same error shape Load returns, so callers can tell "the config is gone"
// from "the config could not be read".
func FileDigest(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &MissingError{Path: path}
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return digestBytes(data), nil
}
