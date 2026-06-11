package offload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mbertschler/squirrel/store"
)

// component is one loaded durability-vector entry: the highest origin
// run covered for an origin node, plus the verification method that
// advanced it. The method lets the gate refuse a presence-only
// component that has no content verification behind it.
type component struct {
	coveredRun int64
	method     string
}

// gate is the offline durability evidence for one invocation: the self
// node (the coordinate content with NULL origin counts under) and one
// durability vector per required target, loaded once up front, plus each
// target's last-successful-whole-volume-push watermark in local run
// space. These locally stored vector rows — including components pulled
// from peers about targets only they can reach — are the entire evidence
// base; the gate makes no network calls.
type gate struct {
	store     *store.Store
	volumeID  int64
	self      store.Node
	require   []string
	vectors   map[string]map[int64]component // target → origin node id → component
	lastPush  map[string]int64               // target → last whole-volume push run (local space)
	nodeNames map[int64]string
}

func loadGate(ctx context.Context, s *store.Store, volumeID int64, require []string) (*gate, error) {
	self, err := s.GetSelfNode(ctx)
	if err != nil {
		return nil, fmt.Errorf("lookup self node: %w", err)
	}
	g := &gate{
		store:     s,
		volumeID:  volumeID,
		self:      self,
		require:   require,
		vectors:   make(map[string]map[int64]component, len(require)),
		lastPush:  make(map[string]int64, len(require)),
		nodeNames: map[int64]string{self.ID: self.Name},
	}
	for _, target := range require {
		components, err := s.ListDestinationRunIDs(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load durability vector for %q: %w", target, err)
		}
		vector := make(map[int64]component, len(components))
		for _, c := range components {
			vector[c.OriginNodeID] = component{coveredRun: c.OriginRunID, method: c.VerifyMethod}
		}
		g.vectors[target] = vector

		push, err := s.LastSuccessfulWholeVolumePushRunID(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load last push watermark for %q: %w", target, err)
		}
		g.lastPush[target] = push
	}
	return g, nil
}

// check evaluates the gate for one present row. Content with origin
// (N, r) is durable on a target only when all three conditions hold:
//
//   - origin vector: the target's component for N covers r;
//   - freshness: the target's last successful whole-volume push (local
//     run space) is at or beyond the run in which the path last became
//     present (status_changed_run_id), so a path re-acquired after the
//     last push is held until a fresh push covers it;
//   - method: that component is content-verified, or — for a presence-
//     only content-addressed component — a verified scan-back
//     fingerprint backs the gated object.
//
// The file passes only when every required target satisfies all three.
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
				fmt.Sprintf("%s: stale: have %d need %d (origin %s)", target, comp.coveredRun, originRun, g.nodeName(ctx, originNode)))
			continue
		}
		if reason := g.freshnessFailure(target, row); reason != "" {
			failures = append(failures, reason)
			continue
		}
		verified, err := g.methodVerified(ctx, target, comp, row)
		if err != nil {
			return nil, err
		}
		if !verified {
			failures = append(failures,
				fmt.Sprintf("%s: not content-verified (method %q); a verified fingerprint must back the object before offload", target, displayMethod(comp.method)))
		}
	}
	return failures, nil
}

// freshnessFailure refuses the target when its last successful whole-
// volume push (local run space) predates the run in which the path last
// became present. This is what closes the re-acquisition hole: a path
// deleted, re-introduced, and re-indexed carries a status_changed_run_id
// past the last push, so it is not durable on the target yet regardless
// of the origin-vector state. A row with no recorded
// status_changed_run_id (a pre-v18 row never re-stamped) is treated as
// "became present at first_seen" — the conservative floor.
func (g *gate) freshnessFailure(target string, row store.FileRow) string {
	changed := row.FirstSeenRunID
	if row.StatusChangedRunID.Valid {
		changed = row.StatusChangedRunID.Int64
	}
	push := g.lastPush[target]
	if push < changed {
		return fmt.Sprintf("%s: not freshly pushed: last whole-volume push run %d < became-present run %d", target, push, changed)
	}
	return ""
}

// methodVerified reports whether the target's component for this row
// rests on genuine content verification. A blake3 / peer-blake3 /
// kopia-verify component passes directly. A presence+size component (a
// content-addressed offsite, where crypt hides the content hash) passes
// only once a verified scan-back fingerprint backs the gated object:
// remote_objects must carry a checksum and a verified_at_ns for this
// (content, destination). Any other method (including a size+mtime push
// or an unknown/pre-v19 component) does not gate.
func (g *gate) methodVerified(ctx context.Context, target string, comp component, row store.FileRow) (bool, error) {
	if store.ContentVerifiedMethod(comp.method) {
		return true, nil
	}
	if comp.method != store.VerifyMethodPresenceSize {
		return false, nil
	}
	obj, err := g.store.GetRemoteObject(ctx, row.ContentID, target)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load fingerprint for content %d on %q: %w", row.ContentID, target, err)
	}
	return obj.Checksum.Valid && obj.Checksum.String != "" && obj.VerifiedAtNs.Valid, nil
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
