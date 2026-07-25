package sync

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
)

// Verification methods reported by the curated handlers on
// VerifyResult.Method. They alias the canonical identifiers in store so
// the durability vector's recorded method and the handler's reported
// method are the same strings — store owns them because the offload gate
// reads them to decide whether a component is content-verified.
const (
	// VerifyMethodBlake3 is rclone's end-to-end content check
	// (--checksum --hash blake3).
	VerifyMethodBlake3 = store.VerifyMethodBlake3
	// VerifyMethodSizeMtime is rclone's default comparison, used for
	// --shallow runs and forced by crypt destinations.
	VerifyMethodSizeMtime = store.VerifyMethodSizeMtime
	// VerifyMethodPeer is the node-sync handshake's receiver-side
	// BLAKE3 re-hash of every delivered path.
	VerifyMethodPeer = store.VerifyMethodPeer
	// VerifyMethodKopia is kopia's own repository consistency check
	// (`kopia snapshot verify`).
	VerifyMethodKopia = store.VerifyMethodKopia
	// VerifyMethodPresenceSize is the content-addressed push's check:
	// rclone reported every transfer succeeded, and a follow-up listing
	// confirmed each object and the manifest segment present at the
	// expected size. Presence evidence is weaker than a content check
	// (crypt remotes expose no hashes), so results carrying it stay
	// unverified until the provider-checksum fingerprint pass lands.
	VerifyMethodPresenceSize = store.VerifyMethodPresenceSize
	// VerifyMethodFingerprint upgrades a presence+size component once every
	// object and pack backing the (volume, destination) pair is
	// fingerprint-verified. Minted by capture and by `squirrel verify`.
	VerifyMethodFingerprint = store.VerifyMethodFingerprint
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
// handlers drive. Rclone backs bucket and peer targets; Kopia backs
// kopia targets and is filled in by ToolsFor exactly when a pair needs
// it.
type Tools struct {
	Rclone *Rclone
	Kopia  *Kopia
}

// ToolsFor bundles the wrappers pairs need: the caller's configured
// rclone wrapper plus, when any pair targets a kopia destination, a
// kopia wrapper whose destination config files live next to the
// squirrel config — the same directory rclone.conf is managed in.
func ToolsFor(cfg *config.Config, pairs []Pair, rcl *Rclone) (Tools, error) {
	tools := Tools{Rclone: rcl}
	for _, p := range pairs {
		if p.Destination != nil && p.Destination.Type == "kopia" {
			kop, err := FindKopia(filepath.Dir(cfg.Path))
			if err != nil {
				return Tools{}, err
			}
			tools.Kopia = kop
			break
		}
	}
	return tools, nil
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
	case p.Destination.Type == "kopia":
		if tools.Kopia == nil {
			return nil, fmt.Errorf("destination %q: kopia wrapper is required (build Tools via ToolsFor)", p.Destination.Name)
		}
		return &kopiaHandler{store: s, kopia: tools.Kopia, vol: p.Volume, dest: p.Destination}, nil
	case p.Destination.Layout == config.LayoutPacked:
		if tools.Rclone == nil {
			return nil, fmt.Errorf("destination %q: rclone wrapper is required", p.Destination.Name)
		}
		return &packedHandler{contentPusher{store: s, rcl: tools.Rclone, vol: p.Volume, dest: p.Destination}}, nil
	case p.Destination.Layout == config.LayoutContentAddressed:
		if tools.Rclone == nil {
			return nil, fmt.Errorf("destination %q: rclone wrapper is required", p.Destination.Name)
		}
		return &contentAddressedHandler{contentPusher{store: s, rcl: tools.Rclone, vol: p.Volume, dest: p.Destination}}, nil
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

// finishHandlerRun writes a handler-driven run's terminal state,
// mirroring the rclone scaffold's finishRun contract: a FinishRun failure
// lands on rep.FinishErr so the caller surfaces it next to the push
// outcome. The kopia, content-addressed, and packed handlers share it;
// their file counts ride on rep.Verification.Files.
//
// A preflight refusal (runErr wrapping ErrRefused — a kopia connect
// without --init, a layout guard) overrides the derived status to
// 'refused' via terminalStatus, and rep.Status is updated in place so the
// CLI summary and TUI show the refusal as its own condition rather than a
// generic failure (#157, F26).
//
// rep.Changed rides along as runs.changed_count where the handler set it
// (the content-layout pushes count their manifest delta); kopia leaves it
// unset, since a snapshot summary reports the whole tree and no honest
// changed count — that run keeps the conservative file_count rendering.
func finishHandlerRun(ctx context.Context, s *store.Store, rep *Report, runErr error) {
	if rep.RunID == 0 {
		return
	}
	rep.Status = terminalStatus(rep.Status, runErr)
	errMsg := ""
	if runErr != nil {
		errMsg = runErr.Error()
	}
	if err := finishRunRow(ctx, s, rep.RunID, rep.Status, errMsg, rep.Verification.Files, rep.Changed); err != nil {
		rep.FinishErr = err
	}
}

// rcloneVerification derives the typed durability report for one rclone
// bucket transfer: BLAKE3 end-to-end when the integrity flags were in
// force, rclone's size+mtime comparison otherwise. Only a fully
// successful BLAKE3 run counts as verified.
//
// A run that asked for BLAKE3 but hit rclone's "no hashes in common"
// fallback is downgraded to size+mtime here even though the flags were
// set and rclone exited 0: rclone silently compared by size, so the copy
// was not content-verified and must not advance the durability vector.
func rcloneVerification(dest *config.Destination, opts Options, rep *Report) VerifyResult {
	v := VerifyResult{
		Method: VerifyMethodBlake3,
		Files:  rep.RcloneResult.Transferred + rep.RcloneResult.Checked,
		Bytes:  rep.RcloneResult.Bytes,
	}
	if EffectiveShallow(dest, opts.Shallow) || rep.RcloneResult.HashFallback {
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
