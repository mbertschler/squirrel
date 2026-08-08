package agent

import (
	"context"
	"fmt"
	"maps"
	"reflect"

	"github.com/mbertschler/squirrel/config"
)

// The config keys a reload can and cannot act on, named as an operator
// wrote them in the TOML file so the latch points at the lines they edited
// rather than at Go field names.
//
// The line between the two halves is what this *process* is, versus what it
// *does*. A volume, a destination, a peer node, a snapshot policy, a verify
// cadence — those are policy the agent consults on its next tick, and it can
// start consulting different policy between two ticks without disturbing
// anything in flight. Its listen address, its bearer tokens, its TLS pair,
// its database handle, its node identity, and the scan loop's period are the
// shape of the process itself: rebinding a listener under live peer syncs,
// or re-arming the audit loop's ticker mid-pass, is a different and far more
// delicate problem than swapping a map. Those wait for a restart, and the
// latch says so by name.
const (
	keyVolumes         = "volumes"
	keyDestinations    = "destinations"
	keyNodes           = "nodes"
	keyBackups         = "backups"
	keyDB              = "db"
	keyNodeName        = "node_name"
	keyAgentVerify     = "agent.verify_every"
	keyAgentListen     = "agent.listen"
	keyAgentDB         = "agent.db"
	keyAgentTLS        = "agent.tls"
	keyAgentToken      = "agent.auth.token"
	keyAgentPeers      = "agent.auth.peers"
	keyAgentScanEvery  = "agent.scan_interval"
	keyAgentScanPolicy = "agent.scan_strategy"
)

// configChanges classifies every difference the file on disk introduces:
// applied is what a reload adopts in place, pending what only a restart can
// change. Both come back in a stable order so a latch message and an audit
// note read the same way twice.
//
// The two halves are measured against different baselines, and they have to
// be. Policy is compared against running — the configuration in force right
// now, which previous reloads have already moved. Process keys are compared
// against booted — the file this process actually bound its listener,
// credentials, and loops from, which no reload ever changes. Measuring both
// against running would leave an operator who rotated a token and then
// thought better of it staring at a restart demand for a credential the
// listener was already using.
//
// Comparison is by value throughout — a config whose file was reformatted,
// commented, or reordered resolves to the same entities and lands in
// neither list, so a cosmetic edit reloads to nothing and latches nothing.
func configChanges(running, booted, next *config.Config) (applied, pending []string) {
	if !reflect.DeepEqual(running.Volumes, next.Volumes) {
		applied = append(applied, keyVolumes)
	}
	if !reflect.DeepEqual(running.Destinations, next.Destinations) {
		applied = append(applied, keyDestinations)
	}
	if !reflect.DeepEqual(running.Nodes, next.Nodes) {
		applied = append(applied, keyNodes)
	}
	if running.Backups != next.Backups {
		applied = append(applied, keyBackups)
	}
	if agentBlock(running).VerifyEvery != agentBlock(next).VerifyEvery {
		applied = append(applied, keyAgentVerify)
	}
	if booted.DB != next.DB {
		pending = append(pending, keyDB)
	}
	if booted.NodeName != next.NodeName {
		pending = append(pending, keyNodeName)
	}
	pending = append(pending, agentProcessChanges(agentBlock(booted), agentBlock(next))...)
	return applied, pending
}

// agentProcessChanges lists the `[agent]` keys that shape the running
// process rather than its policy, and so cannot be adopted in place.
func agentProcessChanges(before, after config.Agent) []string {
	var pending []string
	if before.Listen != after.Listen {
		pending = append(pending, keyAgentListen)
	}
	if before.DB != after.DB {
		pending = append(pending, keyAgentDB)
	}
	if before.TLSCert != after.TLSCert || before.TLSKey != after.TLSKey {
		pending = append(pending, keyAgentTLS)
	}
	if before.Token != after.Token {
		pending = append(pending, keyAgentToken)
	}
	if !maps.Equal(before.PeerTokens, after.PeerTokens) {
		pending = append(pending, keyAgentPeers)
	}
	if before.ScanInterval != after.ScanInterval {
		pending = append(pending, keyAgentScanEvery)
	}
	if before.ScanStrategy != after.ScanStrategy {
		pending = append(pending, keyAgentScanPolicy)
	}
	return pending
}

// agentBlock is the `[agent]` block, or its zero value when the config
// declares none — so "the block was added" and "the block was removed" both
// compare key by key like any other edit, instead of needing their own case.
func agentBlock(c *config.Config) config.Agent {
	if c.Agent == nil {
		return config.Agent{}
	}
	return *c.Agent
}

// reloader adopts a config edit into the running agent (#204, F9). It is
// the "apply" half of the drift monitor: the monitor decides *that* the
// file changed, this decides what that means and makes it so.
//
// The order is the whole design. Load and validate first, so a file that no
// longer parses cannot touch a running agent; rebuild the derived state
// outside the agent next, so a destination the agent could not actually
// reach never becomes the config it is running; and only then swap, in one
// atomic store that no reader can observe halfway.
type reloader struct {
	live *config.Live
	// booted is the configuration this process started from — the one its
	// listener, credentials, and loops were built out of. Never replaced:
	// it is the baseline the restart-only half of every later edit is
	// measured against.
	booted *config.Config
	// prepare rebuilds config-derived state the agent does not own (the
	// CLI's rclone lookup and rclone.conf). Nil when there is none.
	prepare func(context.Context, *config.Config) error
}

// reloadResult is one reload attempt's outcome: what changed, what of it
// was adopted, and what the operator must still restart for.
type reloadResult struct {
	// cfg is the configuration now in force — the whole file, including the
	// keys whose effect a restart still owes, since the agent parsed all of
	// it. Only ever read on the success path; a refused reload returns the
	// zero result alongside its error.
	cfg *config.Config
	// applied and pending name the changed config keys on either side of
	// the reloadable line. Both empty means the file's bytes changed but
	// nothing it resolves to did — a comment, a reordering, a reformat.
	applied []string
	pending []string
}

// apply loads the config file, works out what the change means, and adopts
// the half it can. A failure at any step before the swap leaves the running
// agent exactly as it was; the error is what the latch will say.
func (r *reloader) apply(ctx context.Context, path string) (reloadResult, error) {
	next, err := config.Load(path)
	if err != nil {
		return reloadResult{}, err
	}
	applied, pending := configChanges(r.live.Get(), r.booted, next)
	if r.prepare != nil {
		if err := r.prepare(ctx, next); err != nil {
			return reloadResult{}, fmt.Errorf("rebuild state derived from the new config: %w", err)
		}
	}
	r.live.Store(next)
	return reloadResult{cfg: next, applied: applied, pending: pending}, nil
}
