package offload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/mbertschler/squirrel/store"
)

// component is one loaded durability-vector entry: the highest origin
// run covered for an origin node, the verification method that advanced
// it, and the provenance of that advance. The method lets the gate
// refuse a presence-only component that has no content verification
// behind it; source is the asserting peer's node id when the component
// was last advanced by a durability pull, and invalid (NULL) for the
// locally-verified class — so the gate's decision names which peer's
// evidence backed it. verifiedAt is the wall-clock of the last write that
// carried a verify method or advanced the run — for a pulled component the
// responder's own instant, capped at now (NULL when unknown) — the basis
// for the time-based staleness policy.
type component struct {
	coveredRun int64
	method     string
	source     sql.NullInt64
	verifiedAt sql.NullInt64
}

// gate is the offline durability evidence for one invocation: the self
// node (the coordinate content with NULL origin counts under) and one
// durability vector per required target, loaded once up front, plus two
// freshness sources per target — the last-successful-whole-volume-push
// watermark in local run space (for a target this node pushes to
// directly) and the pulled origin-space push-freshness coordinates (for a
// relayed target this node never pushes to). These locally stored rows —
// including evidence pulled from peers about targets only they can reach
// — are the entire evidence base; the gate makes no network calls.
type gate struct {
	store     *store.Store
	volumeID  int64
	self      store.Node
	require   []string
	vectors   map[string]map[int64]component // target → origin node id → component
	lastPush  map[string]int64               // target → last whole-volume push run (local space)
	freshness map[string]map[int64]int64     // target → origin node id → pulled push-freshness origin run
	nodeNames map[int64]string
	// nowNs is the invocation's wall-clock, captured once so every
	// candidate's staleness is judged against one instant and tests can
	// inject a fixed time.
	nowNs int64
	// maxEvidenceAge, when positive, refuses a component whose
	// verified_at_ns is older than nowNs − maxEvidenceAge (or unknown).
	// Zero disables the time-based policy — the opt-in default, so the
	// version-vector gate behaves exactly as before.
	maxEvidenceAge time.Duration
	// cadenced marks the required targets that carry an effective local
	// verify cadence (a per-destination verify_every, or the [agent]
	// default). A locally-verified fingerprint-verified component gates
	// only while its destination is re-confirmed on a cadence; a target
	// this node cannot see (a relayed offsite) is absent from the map, and
	// the relayed cadence is enforced at pull time instead. Nil is a valid
	// empty set — no target is treated as cadenced.
	cadenced map[string]bool
}

func loadGate(ctx context.Context, s *store.Store, volumeID int64, require []string, nowNs int64, maxEvidenceAge time.Duration, cadenced map[string]bool) (*gate, error) {
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup self node: %w", err)
	}
	g := &gate{
		store:          s,
		volumeID:       volumeID,
		self:           self,
		require:        require,
		vectors:        make(map[string]map[int64]component, len(require)),
		lastPush:       make(map[string]int64, len(require)),
		freshness:      make(map[string]map[int64]int64, len(require)),
		nodeNames:      map[int64]string{self.ID: self.Name},
		nowNs:          nowNs,
		maxEvidenceAge: maxEvidenceAge,
		cadenced:       cadenced,
	}
	for _, target := range require {
		components, err := s.ListDestinationRunIDs(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load durability vector for %q: %w", target, err)
		}
		vector := make(map[int64]component, len(components))
		for _, c := range components {
			vector[c.OriginNodeID] = component{coveredRun: c.OriginRunID, method: c.VerifyMethod, source: c.SourceNodeID, verifiedAt: c.VerifiedAtNs}
		}
		g.vectors[target] = vector

		push, err := s.LastSuccessfulWholeVolumePushRunID(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load last push watermark for %q: %w", target, err)
		}
		g.lastPush[target] = push

		fresh, err := s.ListDestinationPushFreshness(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load push freshness for %q: %w", target, err)
		}
		coords := make(map[int64]int64, len(fresh))
		for _, f := range fresh {
			coords[f.OriginNodeID] = f.OriginRunID
		}
		g.freshness[target] = coords
	}
	return g, nil
}

// check evaluates the gate for one present row. Content with origin
// (N, r) is durable on a target only when all four conditions hold:
//
//   - origin vector: the target's component for N covers r;
//   - evidence age: when a max evidence age is configured, the
//     component's verified_at_ns is within it — defence-in-depth against
//     a destination that was once durable but has not had its coverage
//     re-confirmed in wall-clock time;
//   - freshness: a successful whole-volume push covers the run in which
//     the path last became present, so a path re-acquired after the last
//     push is held until a fresh push covers it. For a target this node
//     pushes to directly the watermark is the last push in local run
//     space; for a relayed target it never pushes to, the watermark is
//     the pulled origin-space push-freshness coordinate for N;
//   - method: that component is content-verified, or — for a presence-
//     only content-addressed component — a verified scan-back
//     fingerprint backs the gated object.
//
// The file passes only when every required target satisfies all four.
// The returned failures name each failing target and reason; an empty
// slice means the gate passed.
func (g *gate) check(ctx context.Context, row store.FileRow) ([]string, error) {
	originNode, originRun, err := g.origin(ctx, row)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, target := range g.require {
		comp, ok := g.vectors[target][originNode]
		switch {
		case !ok:
			failures = append(failures,
				fmt.Sprintf("%s: missing component for origin %s (need %d)", target, g.nodeName(ctx, originNode), originRun))
			continue
		case comp.coveredRun < originRun:
			failures = append(failures,
				fmt.Sprintf("%s: stale: have %d need %d (origin %s, %s)", target, comp.coveredRun, originRun, g.nodeName(ctx, originNode), g.provenance(ctx, comp)))
			continue
		}
		if reason := g.staleEvidenceFailure(ctx, target, comp); reason != "" {
			failures = append(failures, reason)
			continue
		}
		if reason := g.freshnessFailure(ctx, target, row, originNode, originRun); reason != "" {
			failures = append(failures, reason)
			continue
		}
		verified, err := g.methodVerified(ctx, target, comp, row)
		if err != nil {
			return nil, err
		}
		if !verified {
			failures = append(failures,
				fmt.Sprintf("%s: not content-verified (method %q, %s); a verified fingerprint must back the object before offload", target, displayMethod(comp.method), g.provenance(ctx, comp)))
		}
	}
	return failures, nil
}

// staleEvidenceFailure refuses the target when a max evidence age is
// configured and the component's verification is older than that age in
// wall-clock time — the time-based staleness policy (issue #131), a
// complement to the version-vector coverage check above. It is
// fail-closed: a component whose verified_at_ns is unknown (NULL — a
// pre-v23 row, or one only ever advanced methodlessly at its recorded
// run) is treated as infinitely stale and refused, since the gate cannot
// show the coverage was recently re-confirmed.
//
// For a peer-asserted component (a durability pull tagged it) the
// verified_at_ns this node holds is the responder's own verification
// instant, relayed on the pull and capped at now — so the policy bounds
// freshness by when the peer last genuinely re-verified, not merely when
// this node last heard from it. A destination gone dead behind a peer
// that keeps answering ages out here too, and a peer that falls silent
// still ages out because no newer assertion arrives to refresh it. A zero
// maxEvidenceAge disables the policy entirely.
func (g *gate) staleEvidenceFailure(ctx context.Context, target string, comp component) string {
	if g.maxEvidenceAge <= 0 {
		return ""
	}
	cutoff := g.nowNs - int64(g.maxEvidenceAge)
	if !comp.verifiedAt.Valid {
		return fmt.Sprintf("%s: stale evidence: never re-verified (max age %s, %s)",
			target, g.maxEvidenceAge, g.provenance(ctx, comp))
	}
	if comp.verifiedAt.Int64 < cutoff {
		age := time.Duration(g.nowNs - comp.verifiedAt.Int64)
		return fmt.Sprintf("%s: stale evidence: last verified %s ago > max age %s (%s)",
			target, age.Round(time.Second), g.maxEvidenceAge, g.provenance(ctx, comp))
	}
	return ""
}

// freshnessFailure refuses the target when no successful whole-volume
// push covers the run in which the path last became present, closing the
// re-acquisition hole: a path deleted, re-introduced, and re-indexed must
// not be claimed durable on the strength of an origin-vector component
// alone.
//
// Two coordinate spaces, by whether this node pushes to the target
// directly:
//
//   - Local push (lastPush > 0): the watermark is the last successful
//     whole-volume push in local run space, compared against the path's
//     status_changed_run_id. A row with no recorded status_changed_run_id
//     (a pre-v18 row never re-stamped) is treated as "became present at
//     first_seen" — the conservative floor.
//   - Relayed target (no local push): the watermark is the pulled
//     origin-space push-freshness coordinate for the content's origin
//     node, compared against the content's origin run. The pushing node
//     determines freshness in its own run space and reports the maxima of
//     its latest whole-volume push per origin; the gate compares the
//     gated content's origin run against it. Absence of freshness
//     evidence refuses — a relayed target with no recorded push never
//     gates.
func (g *gate) freshnessFailure(ctx context.Context, target string, row store.FileRow, originNode, originRun int64) string {
	if g.lastPush[target] > 0 {
		changed := row.FirstSeenRunID
		if row.StatusChangedRunID.Valid {
			changed = row.StatusChangedRunID.Int64
		}
		if g.lastPush[target] < changed {
			return fmt.Sprintf("%s: not freshly pushed: last whole-volume push run %d < became-present run %d", target, g.lastPush[target], changed)
		}
		return ""
	}
	fresh, ok := g.freshness[target][originNode]
	if !ok {
		return fmt.Sprintf("%s: not freshly pushed: no whole-volume push freshness for origin %s (need %d)",
			target, g.nodeName(ctx, originNode), originRun)
	}
	if fresh < originRun {
		return fmt.Sprintf("%s: not freshly pushed: push freshness %d < origin run %d (origin %s)",
			target, fresh, originRun, g.nodeName(ctx, originNode))
	}
	return ""
}

// methodVerified reports whether the target's component for this row
// rests on genuine content verification. A blake3 / peer-blake3 /
// kopia-verify component passes directly. A presence+size component (a
// content-addressed or packed offsite, where crypt hides the content hash)
// passes only once a verified scan-back fingerprint backs the gated content
// — via either layout: a remote_objects row (per-hash object) or a
// remote_packs row for the content's pack, each carrying a checksum and a
// verified_at_ns for this destination.
//
// A fingerprint-verified component (the upgrade of a presence+size vector
// once the whole pair carries verified fingerprints) is content-verified
// only while a scheduled verify cadence keeps that evidence fresh:
//
//   - relayed (a durability pull tagged it): acceptance was decided at pull
//     time, where a fingerprint-verified method survives only when the
//     responder relayed a positive verify cadence for the destination
//     (else it is baked down to presence+size, which then needs a local
//     fingerprint this node does not hold). So a relayed fingerprint-
//     verified component passes here directly.
//   - local (this node advanced it): it passes while the destination has a
//     live verify cadence; without one it falls back to the per-content
//     scan-back check, since the node holds the fingerprints itself and
//     re-reads them on every verify pass — the cadence coupling binds the
//     relayed path, where the evidence cannot be re-read locally.
//
// Any other method (a size+mtime push or an unknown/pre-v19 component)
// does not gate.
func (g *gate) methodVerified(ctx context.Context, target string, comp component, row store.FileRow) (bool, error) {
	if store.ContentVerifiedMethod(comp.method) {
		return true, nil
	}
	if comp.method == store.VerifyMethodFingerprint {
		if comp.source.Valid || g.cadenced[target] {
			return true, nil
		}
		return g.contentFingerprintVerified(ctx, target, row)
	}
	if comp.method != store.VerifyMethodPresenceSize {
		return false, nil
	}
	return g.contentFingerprintVerified(ctx, target, row)
}

// contentFingerprintVerified reports whether a verified provider
// fingerprint backs the row's content on the target (either layout).
func (g *gate) contentFingerprintVerified(ctx context.Context, target string, row store.FileRow) (bool, error) {
	verified, err := g.store.ContentFingerprintVerified(ctx, row.ContentID, target)
	if err != nil {
		return false, fmt.Errorf("load fingerprint for content %d on %q: %w", row.ContentID, target, err)
	}
	return verified, nil
}

// displayMethod renders a possibly-empty method for a failure message.
func displayMethod(method string) string {
	if method == "" {
		return "unknown"
	}
	return method
}

// origin resolves the row's content to its origin coordinate (node,
// run). Content with a recorded origin uses it verbatim; content with
// NULL (or partially NULL) origin is locally introduced and counts
// under the self node at its introduction run — the content's earliest
// first_seen_run_id in the volume, the same coordinate
// AdvanceDestinationVector and the peer-sync sender use, so the gate
// compares against exactly what the vectors were advanced with.
func (g *gate) origin(ctx context.Context, row store.FileRow) (int64, int64, error) {
	if row.OriginNodeID.Valid && row.OriginRunID.Valid {
		return row.OriginNodeID.Int64, row.OriginRunID.Int64, nil
	}
	intro, err := g.store.ContentIntroductionRunID(ctx, g.volumeID, row.ContentID)
	if err != nil {
		return 0, 0, fmt.Errorf("introduction run for content %d: %w", row.ContentID, err)
	}
	return g.self.ID, intro, nil
}

// provenance renders a component's evidence class for a gate decision:
// "locally verified" for a component this node advanced itself, or
// "asserted by peer <name>" for one a durability pull tagged. The audit
// trail thus names which peer's evidence gated (or failed to gate) an
// offload, and a peer whose assertions later prove untrustworthy is
// identifiable per decision.
func (g *gate) provenance(ctx context.Context, comp component) string {
	if !comp.source.Valid {
		return "locally verified"
	}
	return fmt.Sprintf("asserted by peer %s", g.nodeName(ctx, comp.source.Int64))
}

// nodeName resolves an origin node id to its name for the failure
// messages, cached per invocation. A lookup failure degrades to the
// numeric id — the gate decision is already made, naming is cosmetic.
func (g *gate) nodeName(ctx context.Context, nodeID int64) string {
	if name, ok := g.nodeNames[nodeID]; ok {
		return name
	}
	name := fmt.Sprintf("node-%d", nodeID)
	node, err := g.store.GetNodeByID(ctx, nodeID)
	if err == nil {
		name = node.Name
	} else if !errors.Is(err, sql.ErrNoRows) {
		return name
	}
	g.nodeNames[nodeID] = name
	return name
}
