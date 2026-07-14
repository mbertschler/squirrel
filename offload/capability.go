package offload

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mbertschler/squirrel/config"
)

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
// Only targets with a locally resolved destination config are assessed; a
// required target with no entry in dests (a peer-relayed target this node
// cannot see as a destination) carries no config to analyse and is left to
// the per-file gate, which holds it out until a durability pull covers it.
// A nil map therefore assesses nothing — the pre-check is best-effort over
// whatever config the caller supplies.
func checkTargetsCanGate(require []string, dests map[string]*config.Destination) error {
	var refusals []string
	for _, name := range require {
		d, ok := dests[name]
		if !ok || d == nil {
			// Absent — or present but nil in a partially-constructed map —
			// is left to the per-file gate, never a preflight crash.
			continue
		}
		if capable, reason := d.CanEverGateOffload(); !capable {
			refusals = append(refusals,
				fmt.Sprintf("offload_requires target %q can never satisfy the durability gate (%s)", name, reason))
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return errors.New(strings.Join(refusals, "; "))
}
