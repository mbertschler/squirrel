package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/agent"
	"github.com/mbertschler/squirrel/config"
)

// newAgentCertCmd returns `squirrel agent cert`: a bootstrap helper that
// generates the agent's self-signed TLS certificate + key at the paths the
// [agent.tls] block configures and prints the sha256: pin peers put in
// their [nodes.X.tls] cert_fingerprint (F1). It is a deliberate change (it
// writes key material), not introspection, and refuses to clobber an
// existing certificate without --force: regenerating changes the
// fingerprint and breaks every peer that already pinned it.
func newAgentCertCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "cert",
		Short: "Generate the agent's self-signed TLS cert+key and print its sha256: pin",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runAgentCert(cmd, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing cert/key (changes the fingerprint — every peer must re-pin)")
	return cmd
}

func runAgentCert(cmd *cobra.Command, force bool) error {
	cfg, err := requireConfig(cmd)
	if err != nil {
		return err
	}
	certPath, keyPath, err := agentCertPaths(cfg)
	if err != nil {
		return err
	}
	if err := guardExistingCert(certPath, keyPath, force); err != nil {
		return err
	}
	generated, err := agent.GenerateSelfSignedCert(cfg.NodeName)
	if err != nil {
		return err
	}
	if err := writeCertFiles(certPath, keyPath, generated); err != nil {
		return err
	}
	printCertResult(cmd, cfg, certPath, keyPath, generated.Fingerprint)
	return nil
}

// agentCertPaths pulls the configured cert/key paths, erroring with a
// pointer at the config when the [agent] block or its tls paths are absent —
// there is nowhere to write otherwise.
func agentCertPaths(cfg *config.Config) (certPath, keyPath string, err error) {
	if cfg.Agent == nil {
		return "", "", fmt.Errorf("no [agent] block in %s", cfg.Path)
	}
	if cfg.Agent.TLSCert == "" || cfg.Agent.TLSKey == "" {
		return "", "", fmt.Errorf("no [agent.tls] cert/key configured in %s — set both paths before generating", cfg.Path)
	}
	return cfg.Agent.TLSCert, cfg.Agent.TLSKey, nil
}

// guardExistingCert refuses to overwrite either file unless force is set.
func guardExistingCert(certPath, keyPath string, force bool) error {
	if force {
		return nil
	}
	for _, p := range []string{certPath, keyPath} {
		_, err := os.Stat(p)
		switch {
		case err == nil:
			return fmt.Errorf("%s already exists — refusing to overwrite (pass --force to regenerate; every peer that pinned the old fingerprint must re-pin)", p)
		case !errors.Is(err, os.ErrNotExist):
			return fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return nil
}

// writeCertFiles writes both files owner-only (0600) under 0700 parent
// directories. The certificate is not secret, but keeping it 0600 alongside
// the key is harmless — peers obtain the fingerprint from the pairing helper
// and the startup log, never by reading this file.
func writeCertFiles(certPath, keyPath string, c agent.SelfSignedCert) error {
	for path, data := range map[string][]byte{certPath: c.CertPEM, keyPath: c.KeyPEM} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create directory for %s: %w", path, err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return nil
}

func printCertResult(cmd *cobra.Command, cfg *config.Config, certPath, keyPath, fingerprint string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "wrote certificate %s\n", certPath)
	fmt.Fprintf(out, "wrote private key %s\n\n", keyPath)
	name := cfg.NodeName
	if name == "" {
		name = "<this-node>"
	}
	fmt.Fprintf(out, "Peers pin this node (%s) by adding to their config:\n\n", name)
	fmt.Fprintf(out, "[nodes.%s.tls]\ncert_fingerprint = %q\n", name, fingerprint)
}
