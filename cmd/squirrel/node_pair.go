package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"regexp"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
)

// nodeNameRE mirrors config's volume/destination/node name rule so a bad
// peer name is rejected before it is templated into a TOML key. Kept local
// (config does not export its regexp) and deliberately identical.
var nodeNameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// nodePairOpts carries the peer-side facts this node cannot know on its own,
// supplied via flags; each empty value renders as a clearly-marked
// placeholder the operator fills in.
type nodePairOpts struct {
	localEndpoint   string
	peerEndpoint    string
	peerPath        string
	peerFingerprint string
}

// newNodePairCmd returns `squirrel node pair <peer>`: it generates the two
// bearer tokens a bidirectional peer relationship needs and emits the
// matching config halves for *both* machines, with each token already placed
// in the two slots that must agree. This kills the F3 token-matrix error
// class (four cross-referenced bindings written by hand). It prints only —
// it never edits either config — so the operator reviews and pastes.
func newNodePairCmd() *cobra.Command {
	var opts nodePairOpts
	cmd := &cobra.Command{
		Use:   "pair <peer>",
		Short: "Emit matching config halves (tokens, endpoints, fingerprints) for a peer relationship",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNodePair(cmd, args[0], opts)
		},
	}
	cmd.Flags().StringVar(&opts.localEndpoint, "local-endpoint", "", "this node's agent endpoint as the peer dials it (e.g. https://nas.home:8443)")
	cmd.Flags().StringVar(&opts.peerEndpoint, "peer-endpoint", "", "the peer's agent endpoint")
	cmd.Flags().StringVar(&opts.peerPath, "peer-path", "", "the byte-path where this node mounts the peer's data")
	cmd.Flags().StringVar(&opts.peerFingerprint, "peer-fingerprint", "", "the peer's TLS cert fingerprint (sha256:...)")
	return cmd
}

func runNodePair(cmd *cobra.Command, peer string, opts nodePairOpts) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	local := cfg.NodeName
	if local == "" {
		return fmt.Errorf("node_name is not set in %s — set it before pairing (it names this node to peers)", cfg.Path)
	}
	if !nodeNameRE.MatchString(peer) {
		return fmt.Errorf("peer name %q is invalid (must match %s)", peer, nodeNameRE)
	}
	if peer == local {
		return fmt.Errorf("peer name %q equals this node's name — a node cannot pair with itself", peer)
	}
	// tokenLtoP is presented when the local node calls the peer; tokenPtoL
	// the reverse. Each appears in exactly two slots, cross-matched below.
	tokenLtoP, err := newPairToken()
	if err != nil {
		return err
	}
	tokenPtoL, err := newPairToken()
	if err != nil {
		return err
	}
	printNodePair(cmd.OutOrStdout(), cfg, peer, tokenLtoP, tokenPtoL, opts)
	return nil
}

func printNodePair(out io.Writer, cfg *config.Config, peer, tokenLtoP, tokenPtoL string, opts nodePairOpts) {
	local := cfg.NodeName
	fmt.Fprintf(out, "# Node pairing: %s <-> %s\n", local, peer)
	fmt.Fprintln(out, "# Two bearer tokens were generated and placed so the four bindings already")
	fmt.Fprintln(out, "# cross-match. Paste each half into the named machine's config, fill any")
	fmt.Fprintln(out, "# <...> placeholders, then restart both agents.")
	fmt.Fprintln(out)

	fmt.Fprintf(out, "# ===== on %s (this machine) — add to %s =====\n\n", local, cfg.Path)
	writeNodeBlock(out, peer, placeholder(opts.peerEndpoint, "https://<"+peer+"-host>:8443"),
		placeholder(opts.peerPath, "<path where "+local+" mounts "+peer+"'s data>"),
		tokenLtoP, placeholder(opts.peerFingerprint, "sha256:<"+peer+"'s fingerprint — run `squirrel agent cert` on "+peer+">"))
	fmt.Fprintf(out, "\n[agent.auth.peers.%s]\nbearer = %q\n\n", peer, tokenPtoL)

	fmt.Fprintf(out, "# ===== on %s (the peer) — add to its config =====\n\n", peer)
	writeNodeBlock(out, local, placeholder(opts.localEndpoint, localEndpointGuess(cfg)),
		"<path where "+peer+" mounts "+local+"'s data>",
		tokenPtoL, localFingerprint(cfg))
	fmt.Fprintf(out, "\n[agent.auth.peers.%s]\nbearer = %q\n", local, tokenLtoP)
}

// writeNodeBlock renders one [nodes.<name>] block with its auth + tls
// sub-tables.
func writeNodeBlock(out io.Writer, name, endpoint, path, bearer, fingerprint string) {
	fmt.Fprintf(out, "[nodes.%s]\n", name)
	fmt.Fprintf(out, "endpoint = %q\n", endpoint)
	fmt.Fprintf(out, "path     = %q\n", path)
	fmt.Fprintf(out, "[nodes.%s.auth]\n", name)
	fmt.Fprintf(out, "bearer = %q\n", bearer)
	fmt.Fprintf(out, "[nodes.%s.tls]\n", name)
	fmt.Fprintf(out, "cert_fingerprint = %q\n", fingerprint)
}

// localFingerprint returns this node's cert fingerprint when a cert already
// exists at the configured path, else a placeholder pointing at `agent cert`.
func localFingerprint(cfg *config.Config) string {
	if cfg.Agent == nil || cfg.Agent.TLSCert == "" {
		return "sha256:<this node has no [agent.tls] cert configured>"
	}
	fp, err := agent.FingerprintCertFile(cfg.Agent.TLSCert)
	if err != nil {
		return "sha256:<run `squirrel agent cert` on " + cfg.NodeName + " first>"
	}
	return fp
}

// localEndpointGuess suggests this node's dialable endpoint from the agent's
// listen port; the host is unknowable from a bind address, so it stays a
// placeholder the operator completes.
func localEndpointGuess(cfg *config.Config) string {
	port := "8443"
	if cfg.Agent != nil && cfg.Agent.Listen != "" {
		if _, p, ok := splitHostPort(cfg.Agent.Listen); ok && p != "" {
			port = p
		}
	}
	return "https://<" + cfg.NodeName + "-host>:" + port
}

// splitHostPort splits "host:port" without erroring on a bare ":port" or an
// unbracketed IPv6, which net.SplitHostPort rejects; we only need the port.
func splitHostPort(addr string) (host, port string, ok bool) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], true
		}
	}
	return addr, "", false
}

func placeholder(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

// newPairToken mints a 256-bit URL-safe bearer token.
func newPairToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate pairing token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
