package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/mbertschler/squirrel/config"
)

// errUnknownHostKey refuses an sftp server whose host key known_hosts does
// not pin.
var errUnknownHostKey = errors.New("the sftp server's host key is not in known_hosts")

// errHostKeyChanged refuses an sftp server whose host key differs from
// the one known_hosts pins for it.
var errHostKeyChanged = errors.New("the sftp server's host key differs from the one known_hosts pins")

// knownHostsFile is where dest's pinned host keys live: known_hosts_file,
// else ~/.ssh/known_hosts.
func knownHostsFile(dest *config.Destination) (string, error) {
	if f := dest.Params["known_hosts_file"]; f != "" {
		return expandHome(f), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find ~/.ssh/known_hosts (set known_hosts_file instead): %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// hostKeys is the host key check for one destination: the keys its
// known_hosts file pins.
type hostKeys struct {
	file string
	// known is knownhosts' own check, which reports an unknown host as a
	// KeyError listing no key.
	known ssh.HostKeyCallback
}

// loadHostKeys reads dest's known_hosts. A missing file pins nothing, so
// every host is unknown.
func loadHostKeys(dest *config.Destination) (hostKeys, error) {
	file, err := knownHostsFile(dest)
	if err != nil {
		return hostKeys{}, err
	}
	known, err := knownhosts.New(file)
	if errors.Is(err, fs.ErrNotExist) {
		known = func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }
	} else if err != nil {
		return hostKeys{}, fmt.Errorf("read known_hosts %s: %w", file, err)
	}
	return hostKeys{file: file, known: known}, nil
}

// check accepts only a host key known_hosts pins for the server. An
// unknown host is refused with its fingerprint and the line that would
// trust it.
func (h hostKeys) check(host string, remote net.Addr, key ssh.PublicKey) error {
	err := h.known(host, remote, key)
	ke, ok := errors.AsType[*knownhosts.KeyError](err)
	switch {
	case !ok:
		return err
	case len(ke.Want) == 0:
		return fmt.Errorf("%w: %s presented %s key %s, and squirrel refuses hosts it does not know — if that is the server's fingerprint, trust it by adding this line to %s:\n%s",
			errUnknownHostKey, host, key.Type(), ssh.FingerprintSHA256(key), h.file, knownhosts.Line([]string{knownhosts.Normalize(host)}, key))
	default:
		return fmt.Errorf("%w: %s presented %s key %s, but %s pins another key for it — the server was reinstalled, or something is intercepting the connection; confirm the new key with the server's operator before replacing the entry",
			errHostKeyChanged, host, key.Type(), ssh.FingerprintSHA256(key), h.file)
	}
}

// algorithms lists the host key algorithms of the keys known_hosts pins
// for host, so the handshake asks the server for a key squirrel can check.
// It returns nil, the client's defaults, when known_hosts pins nothing for
// host.
func (h hostKeys) algorithms(host string, remote net.Addr) []string {
	probe, err := probeKey()
	if err != nil {
		return nil
	}
	ke, ok := errors.AsType[*knownhosts.KeyError](h.known(host, remote, probe))
	if !ok {
		return nil
	}
	var algos []string
	for _, k := range ke.Want {
		algos = append(algos, algorithmsFor(k.Key.Type())...)
	}
	return algos
}

// probeKey is a fresh key no known_hosts file holds: checking it against
// a host yields the keys known_hosts pins for that host.
func probeKey() (ssh.PublicKey, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return ssh.NewPublicKey(pub)
}

// algorithmsFor names the signature algorithms a host key of keyType
// signs with.
func algorithmsFor(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}
