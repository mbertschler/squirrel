package config

import (
	"bytes"
	"os"
	"testing"
	"time"
)

const digestBody = `
[volumes.pictures]
path = "/tmp/pictures"
`

// TestLoadRecordsContentDigest is the contract the agent's drift check
// rests on: Load hashes the exact bytes it parsed, and FileDigest reading
// the same file agrees with it.
func TestLoadRecordsContentDigest(t *testing.T) {
	p := writeConfig(t, digestBody)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Digest) != DigestLen {
		t.Fatalf("Digest length = %d, want %d", len(cfg.Digest), DigestLen)
	}
	fromFile, err := FileDigest(p)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	if !bytes.Equal(cfg.Digest, fromFile) {
		t.Fatalf("FileDigest = %x, want Load's %x — the two readers must agree", fromFile, cfg.Digest)
	}
}

// TestFileDigestIgnoresSameBytesRewrite is the "compare content, not
// timestamps" requirement: rewriting the file with identical bytes (a
// config-management tool re-rendering a template, an editor saving an
// unmodified buffer) advances mtime but must not read as a change.
func TestFileDigestIgnoresSameBytesRewrite(t *testing.T) {
	p := writeConfig(t, digestBody)
	before, err := FileDigest(p)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	beforeStat, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.WriteFile(p, []byte(digestBody), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	// Force a distinct mtime so the assertion can't pass by accident on a
	// coarse-grained filesystem clock.
	future := beforeStat.ModTime().Add(time.Hour)
	if err := os.Chtimes(p, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	after, err := FileDigest(p)
	if err != nil {
		t.Fatalf("FileDigest after rewrite: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("digest changed on a same-bytes rewrite: %x → %x", before, after)
	}
}

// TestFileDigestChangesWithContent is the other half: any edit at all, even
// one that resolves to the same Config, changes the digest.
func TestFileDigestChangesWithContent(t *testing.T) {
	p := writeConfig(t, digestBody)
	before, err := FileDigest(p)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	if err := os.WriteFile(p, []byte(digestBody+"\n# a comment\n"), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	after, err := FileDigest(p)
	if err != nil {
		t.Fatalf("FileDigest after edit: %v", err)
	}
	if bytes.Equal(before, after) {
		t.Fatalf("digest unchanged after editing the file: %x", after)
	}
}

// TestFileDigestMissingFile reports the same error shape Load does, so a
// caller can tell "the config is gone" from "the config is unreadable".
func TestFileDigestMissingFile(t *testing.T) {
	_, err := FileDigest(writeConfig(t, digestBody) + ".nope")
	if !IsMissing(err) {
		t.Fatalf("FileDigest on a missing file = %v, want a MissingError", err)
	}
}
