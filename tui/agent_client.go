package tui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/mbertschler/squirrel/config"
)

// errNoAgent is returned by agentClient methods when the TUI was launched
// without an [agent] config block. Browse and history work fine without an
// agent — only write actions surface this error to the user.
var errNoAgent = errors.New("no agent configured: add an [agent] block to ~/.squirrel/config.toml to kick runs from the TUI")

// agentClient is a thin HTTP client for the local agent. v1 only needs a
// "ping" probe so the Dashboard can show "agent: up/down"; the write
// endpoints (kick index, kick sync, cancel run) will be added once the
// matching routes land on the agent side.
type agentClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

func newAgentClient(cfg *config.Config) *agentClient {
	if cfg == nil || cfg.Agent == nil || cfg.Agent.Listen == "" {
		return &agentClient{}
	}
	scheme := "http"
	if cfg.Agent.TLSCert != "" {
		scheme = "https"
	}
	return &agentClient{
		baseURL: fmt.Sprintf("%s://%s", scheme, cfg.Agent.Listen),
		token:   cfg.Agent.Token,
		hc:      &http.Client{Timeout: 2 * time.Second},
	}
}

// configured reports whether the TUI has an agent to talk to. Browse mode
// works without one; write actions need it.
func (c *agentClient) configured() bool {
	return c.baseURL != ""
}

// health probes /v1/health. A nil error means the agent is reachable and
// returned 2xx; any other outcome — DNS failure, refused connection,
// timeout, non-2xx — returns a non-nil error which the caller renders as
// "agent: down".
func (c *agentClient) health(ctx context.Context) error {
	if !c.configured() {
		return errNoAgent
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("agent health: %s", resp.Status)
	}
	return nil
}
