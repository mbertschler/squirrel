package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Node is a row in the `nodes` table. Endpoint and PublicKeyFingerprint
// are nullable: the self-row (the one representing this binary's
// identity) has both NULL; peer rows carry the endpoint URL the
// initiator side declared, and may carry a cryptographic fingerprint
// once that future feature lands.
type Node struct {
	ID                   int64
	Name                 string
	Endpoint             sql.NullString
	PublicKeyFingerprint sql.NullString
}

// GetSelfNode returns the self-row: the single nodes row with
// endpoint IS NULL inserted by the v6 migration. It is the identity
// the agent presents to incoming peers and the FK target for
// locally-written file provenance (today's only path uses NULL on
// the file row, but a future #14-style dedup path may attribute
// rebuilds to self).
func (s *Store) GetSelfNode(ctx context.Context) (Node, error) {
	var n Node
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, endpoint, public_key_fingerprint
		 FROM nodes WHERE endpoint IS NULL ORDER BY id LIMIT 1`).
		Scan(&n.ID, &n.Name, &n.Endpoint, &n.PublicKeyFingerprint)
	return n, err
}

// GetNodeByID returns the node row with the given id, or
// sql.ErrNoRows. The id is the surrogate key used by `contents.origin_node_id`
// and `runs.peer_node_id`.
func (s *Store) GetNodeByID(ctx context.Context, id int64) (Node, error) {
	var n Node
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, endpoint, public_key_fingerprint
		 FROM nodes WHERE id = ?`, id).
		Scan(&n.ID, &n.Name, &n.Endpoint, &n.PublicKeyFingerprint)
	return n, err
}

// GetNodeByName returns the node row with the given name, or
// sql.ErrNoRows. Names are UNIQUE per the schema.
func (s *Store) GetNodeByName(ctx context.Context, name string) (Node, error) {
	var n Node
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, endpoint, public_key_fingerprint
		 FROM nodes WHERE name = ?`, name).
		Scan(&n.ID, &n.Name, &n.Endpoint, &n.PublicKeyFingerprint)
	return n, err
}

// CreateNode inserts a new node row with the given name and optional
// endpoint URL. A non-empty endpoint marks the row as a peer (vs. the
// self-row which has endpoint NULL); same UNIQUE-name behaviour as
// volumes.
func (s *Store) CreateNode(ctx context.Context, name, endpoint string) (Node, error) {
	if !nodeNameRE.MatchString(name) {
		return Node{}, fmt.Errorf("invalid node name %q (must match %s)", name, nodeNameRE)
	}
	var endpointVal sql.NullString
	if endpoint != "" {
		endpointVal = sql.NullString{String: endpoint, Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, endpoint, public_key_fingerprint) VALUES (?, ?, NULL)`,
		name, endpointVal)
	if err != nil {
		return Node{}, fmt.Errorf("insert node %q: %w", name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, fmt.Errorf("node last insert id: %w", err)
	}
	return Node{ID: id, Name: name, Endpoint: endpointVal}, nil
}

// ValidNodeName reports whether name satisfies the node-name rule
// (nodeNameRE). Exposed so wire-facing layers can validate
// peer-declared node names before handing them to CreateNode, failing
// the request instead of surfacing a store error mid-commit.
func ValidNodeName(name string) bool {
	return nodeNameRE.MatchString(name)
}

// GetOrCreateOriginNode resolves a node *name* — the cross-node
// identity content origins travel under — to a local nodes row,
// creating one on first contact. Unlike GetOrCreatePeerNode it matches
// purely by name: a forwarded origin may name the self-row, a known
// peer, or a node this host has never peered with. Created rows carry
// the same "peer://<name>" placeholder endpoint the peer-sync handshake
// records for initiators that expose no URL, so a later real handshake
// under the same name finds a row it agrees with.
func (s *Store) GetOrCreateOriginNode(ctx context.Context, name string) (Node, error) {
	existing, err := s.GetNodeByName(ctx, name)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Node{}, fmt.Errorf("lookup origin node: %w", err)
	}
	return s.CreateNode(ctx, name, "peer://"+name)
}

// GetOrCreatePeerNode looks up a peer node by name. If absent, a new
// row is inserted with the supplied endpoint. If present, the
// endpoint must agree with the stored value: a name re-used across
// peers with different endpoints is a configuration drift we'd
// rather refuse than silently accept (the bearer token is per-peer,
// so a name collision with a real peer would also be an auth
// boundary issue).
//
// The self-row is intentionally NOT returned by this function — its
// endpoint is NULL, and a peer claiming the self-name would be
// caught here by the "different endpoint" comparison (NULL vs.
// non-empty) and rejected.
func (s *Store) GetOrCreatePeerNode(ctx context.Context, name, endpoint string) (Node, error) {
	if endpoint == "" {
		return Node{}, errors.New("peer endpoint must not be empty")
	}
	existing, err := s.GetNodeByName(ctx, name)
	if err == nil {
		if !existing.Endpoint.Valid {
			return Node{}, fmt.Errorf("node %q is the local self-row; refusing to overwrite with peer endpoint %q", name, endpoint)
		}
		if existing.Endpoint.String != endpoint {
			return Node{}, fmt.Errorf("node %q already has endpoint %q in the local index; peer presented %q — resolve the collision before continuing",
				name, existing.Endpoint.String, endpoint)
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Node{}, fmt.Errorf("lookup peer node: %w", err)
	}
	return s.CreateNode(ctx, name, endpoint)
}
