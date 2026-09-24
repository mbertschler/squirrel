package sync

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"path"
	"regexp"
	"strings"

	"github.com/zeebo/blake3"
)

// errNoServerHash marks a destination that offers squirrel no hash of its
// own for a file: a local disk, whose bytes squirrel reads back instead, an
// sftp server that runs no hash command, or a name the command line may
// not carry.
var errNoServerHash = errors.New("the destination offers no hash of its own")

// serverHashCommands are the commands an sftp server runs for each
// hash_algo squirrel's own transport supports.
var serverHashCommands = map[string]string{
	"md5":    "md5sum",
	"sha1":   "sha1sum",
	"sha256": "sha256sum",
	"blake3": "b3sum",
}

// newArtifactHash is the hash algo names, computed on this machine: what
// the server's command must agree with.
func newArtifactHash(algo string) hash.Hash {
	switch algo {
	case "md5":
		return md5.New()
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	case "blake3":
		return blake3.New()
	}
	return nil
}

// commandSafe matches what a server command line may carry unquoted.
var commandSafe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// serverHashProbe is the input the probe hashes on the server.
const serverHashProbe = "squirrel server hash probe\n"

// maxHashOutput bounds what squirrel reads from one hash command.
const maxHashOutput = 4 << 10

// ServerHash runs the configured hash command on the server over the file
// at name. The command line is built from the destination root and name
// alone, and only when both hold nothing but letters, digits, '.', '_', '-'
// and '/' and name ends in a lowercase hex artifact name — never from a
// mirror path, which is a user's file name. The command is probed once per
// session, by hashing a known input; a server without it gets
// errNoServerHash.
func (t *sftpTransport) ServerHash(_ context.Context, name string) (remoteChecksum, error) {
	if err := t.checkName(name); err != nil {
		return remoteChecksum{}, err
	}
	target, err := t.hashTarget(name)
	if err != nil {
		return remoteChecksum{}, err
	}
	if err := t.probeServerHash(); err != nil {
		return remoteChecksum{}, err
	}
	out, err := t.runCommand(serverHashCommands[t.hashAlgo]+" "+target, nil)
	if err != nil {
		return remoteChecksum{}, err
	}
	sum, err := parseHashOutput(out, newArtifactHash(t.hashAlgo).Size())
	if err != nil {
		return remoteChecksum{}, fmt.Errorf("hash %s on the server: %w", name, err)
	}
	return remoteChecksum{Algo: t.hashAlgo, Value: sum}, nil
}

// hashTarget is the command-line argument naming name on the server, or
// errNoServerHash when the command line may not carry it.
func (t *sftpTransport) hashTarget(name string) (string, error) {
	if _, ok := serverHashCommands[t.hashAlgo]; !ok {
		return "", fmt.Errorf("%w: no hash_algo names a command squirrel runs", errNoServerHash)
	}
	full := path.Join(t.root, name)
	if !commandSafe.MatchString(full) || !isStagingKey(path.Base(name)) {
		return "", fmt.Errorf("%w: %s holds characters squirrel never puts on a server command line", errNoServerHash, full)
	}
	if !strings.HasPrefix(full, "/") {
		full = "./" + full
	}
	return full, nil
}

// probeServerHash asks the server, once, to hash a known input, and
// remembers whether its command agreed with this machine.
func (t *sftpTransport) probeServerHash() error {
	t.probeMu.Lock()
	defer t.probeMu.Unlock()
	if !t.probed {
		t.probeErr = t.runProbe()
		t.probed = true
	}
	return t.probeErr
}

func (t *sftpTransport) runProbe() error {
	h := newArtifactHash(t.hashAlgo)
	h.Write([]byte(serverHashProbe))
	command := serverHashCommands[t.hashAlgo]
	out, err := t.runCommand(command, strings.NewReader(serverHashProbe))
	if err != nil {
		return fmt.Errorf("%w: %w", errNoServerHash, err)
	}
	sum, err := parseHashOutput(out, h.Size())
	if err != nil || sum != hex.EncodeToString(h.Sum(nil)) {
		return fmt.Errorf("%w: %s on the server does not compute %s", errNoServerHash, command, t.hashAlgo)
	}
	return nil
}

// runCommand runs cmd in a session of its own and returns what it printed.
func (t *sftpTransport) runCommand(cmd string, stdin io.Reader) (string, error) {
	s, err := t.conn.NewSession()
	if err != nil {
		return "", fmt.Errorf("open a session for %s: %w", cmd, err)
	}
	defer func() { _ = s.Close() }()
	var out bytes.Buffer
	s.Stdin = stdin
	s.Stdout = &limitedWriter{w: &out, n: maxHashOutput}
	if err := s.Run(cmd); err != nil {
		return "", fmt.Errorf("run %s on the server: %w", cmd, err)
	}
	return out.String(), nil
}

// parseHashOutput reads the digest a sum command printed first: size bytes
// of lowercase hex.
func parseHashOutput(out string, size int) (string, error) {
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", errors.New("the command printed nothing")
	}
	sum := strings.ToLower(fields[0])
	if b, err := hex.DecodeString(sum); err != nil || len(b) != size {
		return "", fmt.Errorf("the command printed %q, not a digest", fields[0])
	}
	return sum, nil
}

// limitedWriter keeps the first n bytes written to it and drops the rest.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	keep := min(len(p), l.n)
	if keep > 0 {
		if _, err := l.w.Write(p[:keep]); err != nil {
			return 0, err
		}
		l.n -= keep
	}
	return len(p), nil
}
