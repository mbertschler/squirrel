package sync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/mbertschler/squirrel/config"
)

// sftpConnectTimeout bounds the TCP connect and the ssh handshake of one
// sftp session.
const sftpConnectTimeout = time.Minute

// fsyncExtension is the OpenSSH extension that flushes an open file to
// stable storage.
const fsyncExtension = "fsync@openssh.com"

// dialSFTP opens an sftp session to dest's server and returns the
// transport onto dest's root. The server's host key must be one
// known_hosts pins for it.
func dialSFTP(ctx context.Context, dest *config.Destination) (*sftpTransport, error) {
	keys, err := loadHostKeys(dest)
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", dest.Name, err)
	}
	auth, release, err := sftpAuth(dest)
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", dest.Name, err)
	}
	cfg := &ssh.ClientConfig{User: dest.Params["user"], Auth: auth, HostKeyCallback: keys.check}
	if algos := dest.Params["host_key_algorithms"]; algos != "" {
		cfg.HostKeyAlgorithms = strings.Fields(algos)
	}
	conn, err := dialSSH(ctx, sftpAddress(dest), cfg, keys)
	release()
	if err != nil {
		return nil, fmt.Errorf("destination %q: %w", dest.Name, err)
	}
	client, err := sftp.NewClient(conn, sftp.UseConcurrentWrites(true))
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("destination %q: start sftp: %w", dest.Name, err)
	}
	data, ok := client.HasExtension(fsyncExtension)
	return &sftpTransport{conn: conn, client: client, root: dest.Root, fsync: ok && data == "1"}, nil
}

// sftpAddress is the host:port dest's server listens on.
func sftpAddress(dest *config.Destination) string {
	port := dest.Params["port"]
	if port == "" {
		port = "22"
	}
	return net.JoinHostPort(dest.Params["host"], port)
}

// dialSSH connects to addr and completes the ssh handshake within
// sftpConnectTimeout. Unless the config pins them, the host key
// algorithms follow the keys known_hosts pins, settled once the peer's
// address is known, since known_hosts may pin keys by address.
func dialSSH(ctx context.Context, addr string, cfg *ssh.ClientConfig, keys hostKeys) (*ssh.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, sftpConnectTimeout)
	defer cancel()
	var d net.Dialer
	tcp, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	if cfg.HostKeyAlgorithms == nil {
		cfg.HostKeyAlgorithms = keys.algorithms(addr, tcp.RemoteAddr())
	}
	deadline, _ := ctx.Deadline()
	_ = tcp.SetDeadline(deadline)
	c, chans, reqs, err := ssh.NewClientConn(tcp, addr, cfg)
	if err != nil {
		_ = tcp.Close()
		return nil, fmt.Errorf("ssh to %s: %w", addr, err)
	}
	_ = tcp.SetDeadline(time.Time{})
	return ssh.NewClient(c, chans, reqs), nil
}

// sftpAuth is how squirrel logs in: with the key file and the password
// where configured, else with the keys ssh-agent holds.
func sftpAuth(dest *config.Destination) ([]ssh.AuthMethod, func(), error) {
	var auth []ssh.AuthMethod
	if file := dest.Params["key_file"]; file != "" {
		signer, err := readPrivateKey(file)
		if err != nil {
			return nil, nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	if pw := dest.Params["password"]; pw != "" {
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(answerWith(pw)))
	}
	if len(auth) > 0 {
		return auth, func() {}, nil
	}
	return agentAuth()
}

func readPrivateKey(file string) (ssh.Signer, error) {
	b, err := os.ReadFile(expandHome(file))
	if err != nil {
		return nil, fmt.Errorf("read key_file: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if _, ok := errors.AsType[*ssh.PassphraseMissingError](err); ok {
		return nil, fmt.Errorf("key_file %s is protected by a passphrase, which squirrel cannot enter unattended: load it into ssh-agent and leave key_file and password unset", file)
	}
	if err != nil {
		return nil, fmt.Errorf("parse key_file %s: %w", file, err)
	}
	return signer, nil
}

// answerWith answers every keyboard-interactive prompt with the password,
// for servers that ask for it that way.
func answerWith(pw string) ssh.KeyboardInteractiveChallenge {
	return func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for i := range answers {
			answers[i] = pw
		}
		return answers, nil
	}
}

// agentAuth offers the keys of the ssh-agent at SSH_AUTH_SOCK. The agent
// signs during the handshake, so its connection stays open until release.
func agentAuth() ([]ssh.AuthMethod, func(), error) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, nil, errors.New("no password or key_file is set, and there is no ssh-agent to log in with (SSH_AUTH_SOCK is unset)")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, nil, fmt.Errorf("no password or key_file is set, and the ssh-agent at %s is unreachable: %w", sock, err)
	}
	auth := []ssh.AuthMethod{ssh.PublicKeysCallback(agent.NewClient(c).Signers)}
	return auth, func() { _ = c.Close() }, nil
}

// expandHome expands a leading ~/ to the user's home directory.
func expandHome(p string) string {
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, rest)
}
