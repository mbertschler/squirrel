package offload

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mbertschler/squirrel/store"
)

// gate is the offline durability evidence for one invocation: the self
// node (the coordinate content with NULL origin counts under) and one
// durability vector per required target, loaded once up front. These
// locally stored vector rows — including components pulled from peers
// about targets only they can reach — are the entire evidence base; the
// gate makes no network calls.
type gate struct {
	store     *store.Store
	volumeID  int64
	self      store.Node
	require   []string
	vectors   map[string]map[int64]int64 // target → origin node id → covered origin run
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
		vectors:   make(map[string]map[int64]int64, len(require)),
		nodeNames: map[int64]string{self.ID: self.Name},
	}
	for _, target := range require {
		components, err := s.ListDestinationRunIDs(ctx, volumeID, target)
		if err != nil {
			return nil, fmt.Errorf("load durability vector for %q: %w", target, err)
		}
		vector := make(map[int64]int64, len(components))
		for _, c := range components {
			vector[c.OriginNodeID] = c.OriginRunID
		}
		g.vectors[target] = vector
	}
	return g, nil
}

// check evaluates the gate for one present row: content with origin
// (N, r) is durable on a target iff that target's vector component for
// N is ≥ r, and the file passes only when every required target's
// component does. The returned failures name each failing target with
// the missing or stale component; an empty slice means the gate passed.
func (g *gate) check(ctx context.Context, row store.FileRow) ([]string, error) {
	originNode, originRun, err := g.origin(ctx, row)
	if err != nil {
		return nil, err
	}
	var failures []string
	for _, target := range g.require {
		covered, ok := g.vectors[target][originNode]
		switch {
		case !ok:
			failures = append(failures,
				fmt.Sprintf("%s: missing component for origin %s (need %d)", target, g.nodeName(ctx, originNode), originRun))
		case covered < originRun:
			failures = append(failures,
				fmt.Sprintf("%s: stale: have %d need %d (origin %s)", target, covered, originRun, g.nodeName(ctx, originNode)))
		}
	}
	return failures, nil
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
