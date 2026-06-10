package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Verification methods reported by the curated handlers on
// VerifyResult.Method. Callers key per-tool rendering off these.
const (
	// VerifyMethodBlake3 is rclone's end-to-end content check
	// (--checksum --hash blake3).
	VerifyMethodBlake3 = "blake3"
	// VerifyMethodSizeMtime is rclone's default comparison, used for
	// --shallow runs and forced by crypt destinations.
	VerifyMethodSizeMtime = "size+mtime"
	// VerifyMethodPeer is the node-sync handshake's receiver-side
	// BLAKE3 re-hash of every delivered path.
	VerifyMethodPeer = "peer-blake3"
)

// VerifyResult is the typed durability report of one handler push: how
// the destination's copy was checked and what the tool counted. The
// verified flag is unexported, so a positive result can only be minted
// by the curated handlers in this package — that keeps durability
// reporting structurally separate from the hook mechanism, whose
// outcomes are exit-code-only by design.
type VerifyResult struct {
	verified bool
	// Method names the comparison that backed this push.
	Method string
	// SnapshotID identifies the snapshot for snapshot-based handlers.
	SnapshotID string
	// Files and Bytes are the counts the tool reported for this push.
	Files int64
	Bytes int64
}

// Verified reports whether the destination's copy of this push was
// content-verified.
func (v VerifyResult) Verified() bool { return v.verified }

// Tools bundles the configured external-tool wrappers the curated
// handlers drive. Rclone backs bucket and peer targets.
type Tools struct {
	Rclone *Rclone
}

// Handler is the curated, type-determined driver for one (volume,
// target) pair. A handler owns the external tool invocation end to end
// — verb, safety flags, source/destination composition — so config can
// only supply declarative parameters, never alter the operation.
//
// The interface is sealed: every implementation lives in this package,
// which keeps the ability to produce a VerifyResult a curated-handler
// capability.
type Handler interface {
	// TargetName names the destination or node for run rows and output.
	TargetName() string
	// Push transfers the pair's volume to its target, verifies the
	// result, and records the runs row. The typed durability outcome
	// lands on Report.Verification.
	Push(ctx context.Context, opts Options) (Report, error)

	sealed()
}

// HandlerFor returns the curated handler for p, chosen by the target's
// declared type.
func HandlerFor(s *store.Store, tools Tools, p Pair) (Handler, error) {
	switch {
	case p.IsNode():
		if tools.Rclone == nil {
			return nil, fmt.Errorf("node %q: rclone wrapper is required", p.Node.Name)
		}
		return &peerHandler{store: s, rcl: tools.Rclone, vol: p.Volume, node: p.Node}, nil
	case p.Destination == nil:
		return nil, errors.New("pair names no destination or node")
	default:
		if tools.Rclone == nil {
			return nil, fmt.Errorf("destination %q: rclone wrapper is required", p.Destination.Name)
		}
		return &rcloneHandler{store: s, rcl: tools.Rclone, vol: p.Volume, dest: p.Destination}, nil
	}
}

// rcloneHandler pushes to an rclone-backed bucket destination via Sync.
type rcloneHandler struct {
	store *store.Store
	rcl   *Rclone
	vol   *config.Volume
	dest  *config.Destination
}

func (h *rcloneHandler) TargetName() string { return h.dest.Name }

func (h *rcloneHandler) Push(ctx context.Context, opts Options) (Report, error) {
	return Sync(ctx, h.store, h.rcl, h.vol, h.dest, opts)
}

func (h *rcloneHandler) sealed() {}

// peerHandler pushes to a peer node via the SyncNode handshake.
type peerHandler struct {
	store *store.Store
	rcl   *Rclone
	vol   *config.Volume
	node  *config.Node
}

func (h *peerHandler) TargetName() string { return h.node.Name }

func (h *peerHandler) Push(ctx context.Context, opts Options) (Report, error) {
	return SyncNode(ctx, h.store, h.rcl, h.vol, h.node, opts)
}

func (h *peerHandler) sealed() {}

// rcloneVerification derives the typed durability report for one rclone
// bucket transfer: BLAKE3 end-to-end when the integrity flags were in
// force, rclone's size+mtime comparison otherwise. Only a fully
// successful BLAKE3 run counts as verified.
func rcloneVerification(dest *config.Destination, opts Options, rep *Report) VerifyResult {
	v := VerifyResult{
		Method: VerifyMethodBlake3,
		Files:  rep.RcloneResult.Transferred + rep.RcloneResult.Checked,
		Bytes:  rep.RcloneResult.Bytes,
	}
	if EffectiveShallow(dest, opts.Shallow) {
		v.Method = VerifyMethodSizeMtime
	}
	v.verified = v.Method == VerifyMethodBlake3 && rep.Status == store.RunStatusSuccess
	return v
}

// peerVerification derives the typed durability report for one node
// sync. The receiver re-hashes every delivered path with BLAKE3 during
// the handshake's verify phase, so a fully successful session is
// content-verified even when the rclone transfer itself ran shallow.
func peerVerification(rep *Report) VerifyResult {
	return VerifyResult{
		verified: rep.Status == store.RunStatusSuccess,
		Method:   VerifyMethodPeer,
		Files:    int64(len(rep.NodeVerify.Matched)),
		Bytes:    rep.RcloneResult.Bytes,
	}
}
