package offload

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mbertschler/squirrel/config"
)

// RelayedTargetCapability is a peer-relayed required target's offload-gating
// capability as reported by the peer that owns it, gathered by the caller
// from the durability exchange before Offload runs (#145). A peer-relayed
// target has no local destination config on this node — its durability is
// learned over sync rather than pushed to directly — so this node cannot run
// the CanEverGateOffload predicate itself and instead trusts the owning
// peer's verdict, exactly as it trusts that peer's durability assertions.
//
// Entries are strictly best-effort. A relayed target with no entry (the peer
// was unreachable, or predates the capability field) is left to the per-file
// gate, so absent or stale capability info never turns a genuinely-pending
// state into an abort. Only an entry whose CanGate is false ever aborts.
type RelayedTargetCapability struct {
	// Target is the offload_requires name this capability is about.
	Target string
	// Peer is the owning peer's node name, named in the abort message so
	// the operator knows whose destination is the problem.
	Peer string
	// CanGate is the owning peer's CanEverGateOffload verdict for Target.
	CanGate bool
	// Reason names the structural gap when CanGate is false; empty
	// otherwise.
	Reason string
}

// checkTargetsCanGate refuses the whole offload up front when any required
// target is structurally incapable of ever contributing a durability
// component the gate accepts. offload_requires is a conjunction — a file is
// deletable only when every required target covers it — so a single target
// that can never gate makes every file report not-durable forever. That is
// a policy error, not a pending state, so it is surfaced once as a hard
// abort (no run row opened, no candidates walked) rather than silently per
// file. Every incapable target is named so the operator fixes the policy in
// one pass.
//
// A required target with a locally resolved destination config is assessed
// against that config, which is authoritative for it. A required target with
// no local config is peer-relayed: it is assessed against relayed, the
// owning peer's advertised capability (issue #145). Both sources are
// best-effort — a target absent from dests and from relayed (a peer
// unreachable at pre-check time, or one predating the capability field) is
// left to the per-file gate, which holds it out until a durability pull
// covers it. Nil inputs therefore assess nothing.
func checkTargetsCanGate(require []string, dests map[string]*config.Destination, relayed []RelayedTargetCapability) error {
	incapableRelayed := indexIncapableRelayed(relayed)
	var refusals []string
	for _, name := range require {
		if d, ok := dests[name]; ok && d != nil {
			// A locally-configured target is judged solely by its local
			// config — authoritative, and unchanged from #121/#156. Relayed
			// capability for the same name (if any) is ignored.
			if capable, reason := d.CanEverGateOffload(); !capable {
				refusals = append(refusals,
					fmt.Sprintf("offload_requires target %q can never satisfy the durability gate (%s)", name, reason))
			}
			continue
		}
		// Peer-relayed (or absent locally): only a positive incapable
		// verdict from the owning peer aborts; absent info falls through.
		if rc, ok := incapableRelayed[name]; ok {
			refusals = append(refusals,
				fmt.Sprintf("offload_requires target %q can never satisfy the durability gate (%s), reported by peer %q", name, rc.Reason, rc.Peer))
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return errors.New(strings.Join(refusals, "; "))
}

// indexIncapableRelayed keys the incapable relayed verdicts by target name.
// Only CanGate==false entries are retained — a capable verdict is a pending
// state, never an abort — so a target present in the map is always a
// fail-fast candidate. When more than one peer reports the same target the
// first incapable verdict wins; naming any one owning peer is enough for the
// operator to act.
func indexIncapableRelayed(relayed []RelayedTargetCapability) map[string]RelayedTargetCapability {
	out := make(map[string]RelayedTargetCapability, len(relayed))
	for _, c := range relayed {
		if c.CanGate {
			continue
		}
		if _, ok := out[c.Target]; ok {
			continue
		}
		out[c.Target] = c
	}
	return out
}
