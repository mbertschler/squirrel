package agent

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// HistoryDirName mirrors sync.HistoryDirName at the agent side — the
// reserved directory at the volume root where pre-supersede moves
// stage prior bytes. Lowercase-duplicated here rather than imported to
// keep the agent package free of the sync package's rclone
// dependency.
const HistoryDirName = ".squirrel-history"

// ConflictsDirName mirrors sync.ConflictsDirName: the reserved
// directory at the volume root where conflict pre-stages preserve the
// loser's bytes. Distinct from HistoryDirName so the user can tell
// "this version was overwritten by a normal sync" (history) from "two
// writers diverged at the same path and both versions are preserved"
// (conflicts) without parsing run-id semantics.
const ConflictsDirName = ".squirrel-conflicts"

// peerSyncRouter holds per-server state shared by all peer-sync
// endpoints: the volume-level lock map (one in-flight session per
// volume) and the session table (transient state between /begin and
// /close, keyed by receiver_run_id).
type peerSyncRouter struct {
	srv      *Server
	volumes  map[string]*config.Volume
	mu       sync.Mutex
	locks    map[int64]bool         // volume_id → busy
	sessions map[int64]*peerSession // receiver_run_id → state
}

// peerSession captures everything one in-flight sync run carries
// between the four endpoint calls. Lives in memory; agent restart
// drops all in-flight sessions (acceptable for v1 — the next sync
// replans from scratch).
type peerSession struct {
	receiverRunID   int64
	volume          *config.Volume
	volumeID        int64
	peerNodeID      int64
	correlatedRunID int64
	// dispositions stores the receiver's verdict per path so /verify
	// and /close can rehash and commit without re-running the diff.
	dispositions map[string]*sessionEntry
	// conflictOrder records the conflict-disposition paths in the
	// order the initiator sent them in /plan, so the /plan response
	// (and the CLI rendering downstream) is deterministic instead of
	// reflecting the map iteration order.
	conflictOrder []string
}

// sessionEntry is one path's state across the session: the
// initiator's claim from /plan, used at /verify (to know what hash
// to compare on-disk bytes against) and at /close (to construct the
// new file row). For conflict-disposition paths the entry also
// carries the prior row (so the conflict-path insert keeps the prior
// provenance), the resolved reason string for the wire response, and
// the relative path the prior bytes were moved to.
type sessionEntry struct {
	disposition string
	blake3      []byte
	size        int64
	mtimeNs     int64
	// priorRow is the receiver's pre-stage view of the row at this
	// path. Populated for supersede and conflict; nil otherwise.
	priorRow *store.FileRow
	// conflictReason is the human-readable reason classify decided
	// "conflict", surfaced verbatim in the /plan response.
	conflictReason string
	// preservedAtPath is the volume-relative path the prior bytes
	// were moved to during conflict pre-stage. Empty until pre-stage
	// completes (or for non-conflict dispositions).
	preservedAtPath string
}

func newPeerSyncRouter(srv *Server, volumes map[string]*config.Volume) *peerSyncRouter {
	return &peerSyncRouter{
		srv:      srv,
		volumes:  volumes,
		locks:    make(map[int64]bool),
		sessions: make(map[int64]*peerSession),
	}
}

// register attaches the four /v1/sync/* routes to mux. Health and
// the placeholder /v1/plan stay where buildHandler put them; this
// function is the only place new routes land.
func (r *peerSyncRouter) register(mux *http.ServeMux) {
	mux.Handle("POST /v1/sync/begin", r.srv.requireBearer(http.HandlerFunc(r.handleBegin)))
	mux.Handle("POST /v1/sync/plan", r.srv.requireBearer(http.HandlerFunc(r.handlePlan)))
	mux.Handle("POST /v1/sync/verify", r.srv.requireBearer(http.HandlerFunc(r.handleVerify)))
	mux.Handle("POST /v1/sync/close", r.srv.requireBearer(http.HandlerFunc(r.handleClose)))
}

// acquireVolumeLock takes the per-volume lock or returns false. The
// caller must release on every exit path (success and error).
func (r *peerSyncRouter) acquireVolumeLock(volumeID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.locks[volumeID] {
		return false
	}
	r.locks[volumeID] = true
	return true
}

func (r *peerSyncRouter) releaseVolumeLock(volumeID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.locks, volumeID)
}

func (r *peerSyncRouter) storeSession(s *peerSession) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s.receiverRunID] = s
}

func (r *peerSyncRouter) takeSession(receiverRunID int64) *peerSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[receiverRunID]
	if !ok {
		return nil
	}
	delete(r.sessions, receiverRunID)
	return s
}

func (r *peerSyncRouter) lookupSession(receiverRunID int64) *peerSession {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[receiverRunID]
}

// handleBegin implements POST /v1/sync/begin. The handler is the
// thin HTTP shell over beginSession, which carries the actual flow.
func (r *peerSyncRouter) handleBegin(w http.ResponseWriter, req *http.Request) {
	var body syncproto.BeginRequest
	if err := decodeJSON(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	resp, status, err := r.beginSession(req.Context(), body)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// beginSession is the handler body in plain-function form so each
// failure mode pairs its error with the right HTTP status without
// burying the control flow in repeated writeError/return tuples.
// The lock is acquired before any DB row insertion that would need
// rollback on a later failure; releasing it lives in the per-phase
// guard.
func (r *peerSyncRouter) beginSession(ctx context.Context, body syncproto.BeginRequest) (syncproto.BeginResponse, int, error) {
	if body.Volume == "" || body.InitiatorNodeName == "" || body.InitiatorRunID == 0 {
		return syncproto.BeginResponse{}, http.StatusBadRequest, errors.New("volume, initiator_node_name, and initiator_run_id are required")
	}
	vol, ok := r.volumes[body.Volume]
	if !ok {
		return syncproto.BeginResponse{}, http.StatusNotFound, fmt.Errorf("volume %q is not declared on this node", body.Volume)
	}
	v, err := r.ensureVolumeRow(ctx, body.Volume, vol.Path)
	if err != nil {
		return syncproto.BeginResponse{}, http.StatusInternalServerError, err
	}
	if !r.acquireVolumeLock(v.ID) {
		return syncproto.BeginResponse{}, http.StatusConflict, fmt.Errorf("volume %q already has an in-flight sync", body.Volume)
	}
	resp, status, err := r.finishBegin(ctx, body, vol, v)
	if err != nil {
		r.releaseVolumeLock(v.ID)
	}
	return resp, status, err
}

// ensureVolumeRow looks up the volume by name on the receiver side
// and creates the row on first contact. The config-declared path is
// what we materialise; a subsequent `squirrel index` run on the
// receiver host refills the file rows from disk.
func (r *peerSyncRouter) ensureVolumeRow(ctx context.Context, name, absPath string) (store.Volume, error) {
	v, err := r.srv.store.GetVolumeByName(ctx, name)
	if err == nil {
		return v, nil
	}
	if !store.IsNotFound(err) {
		return store.Volume{}, fmt.Errorf("lookup volume: %w", err)
	}
	created, err := r.srv.store.CreateVolume(ctx, name, absPath)
	if err != nil {
		return store.Volume{}, fmt.Errorf("create volume row: %w", err)
	}
	return created, nil
}

// finishBegin runs the post-lock steps: peer/self lookup, runs-row
// insertion, in-memory session registration. The caller releases the
// volume lock on any non-nil error.
func (r *peerSyncRouter) finishBegin(ctx context.Context, body syncproto.BeginRequest, vol *config.Volume, v store.Volume) (syncproto.BeginResponse, int, error) {
	peer, err := r.srv.store.GetOrCreatePeerNode(ctx, body.InitiatorNodeName, peerEndpoint(body))
	if err != nil {
		return syncproto.BeginResponse{}, http.StatusConflict, err
	}
	self, err := r.srv.store.GetSelfNode(ctx)
	if err != nil {
		return syncproto.BeginResponse{}, http.StatusInternalServerError, fmt.Errorf("look up self node: %w", err)
	}
	runID, err := r.srv.store.BeginPeerSyncRun(ctx, v.ID, peer.ID, body.InitiatorRunID, body.InitiatorNodeName)
	if err != nil {
		return syncproto.BeginResponse{}, http.StatusInternalServerError, fmt.Errorf("begin run: %w", err)
	}
	warnings, err := r.collectDriftWarnings(ctx, body.Volume, v.ID, peer.ID)
	if err != nil {
		return syncproto.BeginResponse{}, http.StatusInternalServerError, fmt.Errorf("collect drift warnings: %w", err)
	}
	r.storeSession(&peerSession{
		receiverRunID:   runID,
		volume:          vol,
		volumeID:        v.ID,
		peerNodeID:      peer.ID,
		correlatedRunID: body.InitiatorRunID,
		dispositions:    make(map[string]*sessionEntry),
	})
	return syncproto.BeginResponse{
		ReceiverRunID:    runID,
		ReceiverNodeName: self.Name,
		PendingWarnings:  warnings,
	}, http.StatusOK, nil
}

// collectDriftWarnings produces one PendingWarnings line per audit run
// against (volumeID) since the last successful sync with peerNodeID
// that detected non-zero drift. Drift is the sum of `modified`
// (content changed in place — supersede chain) and `missing` (file
// vanished from disk — MarkMissing flip). The watermark is read from
// peer_sync_state.last_synced_at; no row yet means "first contact"
// and surfaces every audit run on the volume.
//
// Both counts are derived from the existing files table; no
// audit-specific schema column carries them. Clean audits (zero
// modified + zero missing) are omitted so the initiator's CLI doesn't
// spam empty lines on every sync.
func (r *peerSyncRouter) collectDriftWarnings(ctx context.Context, volumeName string, volumeID, peerNodeID int64) ([]string, error) {
	state, err := r.srv.store.GetPeerSyncState(ctx, volumeID, peerNodeID)
	var sinceNs int64
	if err == nil {
		sinceNs = state.LastSyncedAtNs
	} else if !store.IsNotFound(err) {
		return nil, fmt.Errorf("peer_sync_state: %w", err)
	}
	audits, err := r.srv.store.ListAuditRunsSince(ctx, volumeID, sinceNs)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, run := range audits {
		modified, err := r.srv.store.CountModifiedFilesByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		missing, err := r.srv.store.CountMissingFilesByRun(ctx, run.ID)
		if err != nil {
			return nil, err
		}
		if modified == 0 && missing == 0 {
			continue
		}
		out = append(out, fmt.Sprintf("audit run %d on volume %s: %d modified, %d missing",
			run.ID, volumeName, modified, missing))
	}
	return out, nil
}

// peerEndpoint resolves the endpoint string to store on the peer
// nodes row. Single-writer initiators don't expose an agent of their
// own, so the empty case yields a stable name-derived placeholder
// that satisfies the "non-empty endpoint" invariant without leaking a
// real URL onto the wire.
func peerEndpoint(body syncproto.BeginRequest) string {
	if body.InitiatorEndpoint != "" {
		return body.InitiatorEndpoint
	}
	return "peer://" + body.InitiatorNodeName
}

// handlePlan implements POST /v1/sync/plan: diff the initiator's index
// slice against the receiver's store and pre-move the supersede paths.
func (r *peerSyncRouter) handlePlan(w http.ResponseWriter, req *http.Request) {
	var body syncproto.PlanRequest
	if err := decodeJSON(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := r.lookupSession(body.ReceiverRunID)
	if sess == nil {
		writeError(w, http.StatusNotFound, "no session for receiver_run_id")
		return
	}
	ctx := req.Context()
	resp, err := r.planSession(ctx, sess, body.Entries)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// planSession computes the four-bucket diff for the given entries and
// performs pre-moves for the supersede + conflict buckets. Errors here
// roll the session back to a clean state (no partial pre-moves on
// failure). The conflict list is assembled after pre-stage so its
// PreservedAtPath field is populated.
func (r *peerSyncRouter) planSession(ctx context.Context, sess *peerSession, entries []syncproto.IndexEntry) (syncproto.PlanResponse, error) {
	resp := syncproto.PlanResponse{
		Dispositions: make([]syncproto.PlanDisposition, 0, len(entries)),
	}
	for _, e := range entries {
		if err := validateRelPath(e.Path); err != nil {
			return syncproto.PlanResponse{}, fmt.Errorf("path %q: %w", e.Path, err)
		}
		digest, err := hex.DecodeString(e.Blake3Hex)
		if err != nil || len(digest) != 32 {
			return syncproto.PlanResponse{}, fmt.Errorf("invalid blake3 hex %q for path %q", e.Blake3Hex, e.Path)
		}
		entry := &sessionEntry{blake3: digest, size: e.SizeBytes, mtimeNs: e.MtimeNs}
		disp, err := r.classify(ctx, sess, e.Path, entry)
		if err != nil {
			return syncproto.PlanResponse{}, fmt.Errorf("classify %q: %w", e.Path, err)
		}
		entry.disposition = disp
		sess.dispositions[e.Path] = entry
		if disp == syncproto.DispositionConflict {
			sess.conflictOrder = append(sess.conflictOrder, e.Path)
		}
		resp.Dispositions = append(resp.Dispositions, syncproto.PlanDisposition{
			Path: e.Path, Disposition: disp, Blake3Hex: e.Blake3Hex,
		})
	}
	if err := r.preMoveSupersedes(sess); err != nil {
		return syncproto.PlanResponse{}, fmt.Errorf("pre-move supersedes: %w", err)
	}
	if err := r.preStageConflicts(ctx, sess); err != nil {
		return syncproto.PlanResponse{}, fmt.Errorf("pre-stage conflicts: %w", err)
	}
	resp.Conflicts = collectConflicts(sess)
	return resp, nil
}

// validateRelPath rejects wire paths that would escape the volume
// root once joined with filepath.Join (a malicious peer could
// otherwise have the receiver mv files into /etc or overwrite host
// files), or that would land under a reserved sync directory the
// receiver owns. The cleaner-and-prefix-check pattern catches
// "../escape", "a/../b/../etc/passwd", "//etc/passwd",
// and similar variants that filepath.Join itself would resolve to
// outside the root.
func validateRelPath(p string) error {
	if p == "" {
		return errors.New("path must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return errors.New("path must not contain a NUL byte")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return errors.New("path must be relative")
	}
	cleaned := filepath.ToSlash(filepath.Clean(p))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return errors.New("path escapes the volume root")
	}
	if cleaned == HistoryDirName || strings.HasPrefix(cleaned, HistoryDirName+"/") ||
		cleaned == ConflictsDirName || strings.HasPrefix(cleaned, ConflictsDirName+"/") {
		return errors.New("path is under a reserved sync directory")
	}
	return nil
}

// collectConflicts builds the wire-format conflict list from the
// post-pre-stage session entries in the order the initiator sent
// them. Iterating sess.conflictOrder (a slice) instead of
// sess.dispositions (a map) keeps PlanResponse deterministic across
// runs and Go versions.
func collectConflicts(sess *peerSession) []syncproto.ConflictDetail {
	out := make([]syncproto.ConflictDetail, 0, len(sess.conflictOrder))
	for _, path := range sess.conflictOrder {
		entry := sess.dispositions[path]
		out = append(out, syncproto.ConflictDetail{
			Path:               path,
			InitiatorBlake3Hex: hex.EncodeToString(entry.blake3),
			ReceiverBlake3Hex:  hex.EncodeToString(entry.priorRow.Blake3),
			Reason:             entry.conflictReason,
			PreservedAtPath:    entry.preservedAtPath,
		})
	}
	return out
}

// classify is the four-bucket decision per path. Supersede + conflict
// stash the prior row on the session entry so the pre-stage step (run
// after every entry has been classified) doesn't have to re-fetch it.
func (r *peerSyncRouter) classify(ctx context.Context, sess *peerSession, relPath string, entry *sessionEntry) (string, error) {
	existing, err := r.srv.store.GetByPath(ctx, sess.volumeID, relPath)
	if err != nil {
		if store.IsNotFound(err) {
			return syncproto.DispositionTransfer, nil
		}
		return "", err
	}
	if existing.Status != store.StatusPresent {
		// A live row in 'missing' status is treated as "no file here";
		// the initiator's bytes are new content.
		return syncproto.DispositionTransfer, nil
	}
	if bytesEqual(existing.Blake3, entry.blake3) {
		return syncproto.DispositionAlreadyCorrect, nil
	}
	// Different blake3 at this path. Provenance decides the verdict.
	disp, reason := r.dispositionForExisting(ctx, sess, existing)
	entry.priorRow = &existing
	if disp == syncproto.DispositionConflict {
		entry.conflictReason = reason
	}
	return disp, nil
}

// dispositionForExisting is the provenance check that distinguishes
// supersede from conflict (per CLAUDE.md "check authoritative state
// first"). Rules:
//
//   - source_node_id IS NULL → local write on receiver → conflict.
//   - source_node_id != this initiator → another peer wrote it →
//     conflict.
//   - source_node_id == this initiator → supersede, provided the
//     row's correlated initiator run-id is ≤ the per-(volume, peer)
//     watermark. Translating the row's local source_run_id back into
//     the initiator's id space requires looking up the receiver-side
//     runs row and reading its correlated_run_id (the two columns are
//     in different id spaces — receiver-local vs. initiator-local).
//
// All three branches can fire in the multi-writer flow: the
// receiver may have local writes (a NAS web app dropped a file in,
// or `squirrel index` ran on the receiver host between syncs), or
// it may carry rows from a different peer that haven't synced
// through us yet.
func (r *peerSyncRouter) dispositionForExisting(ctx context.Context, sess *peerSession, existing store.FileRow) (string, string) {
	if !existing.SourceNodeID.Valid {
		return syncproto.DispositionConflict, "local write on receiver"
	}
	if existing.SourceNodeID.Int64 != sess.peerNodeID {
		return syncproto.DispositionConflict, "sourced from a different peer"
	}
	state, err := r.srv.store.GetPeerSyncState(ctx, sess.volumeID, sess.peerNodeID)
	if err != nil && !store.IsNotFound(err) {
		return syncproto.DispositionConflict, fmt.Sprintf("peer_sync_state lookup error: %v", err)
	}
	// No watermark yet (first sync with this peer): the only way a
	// peer-sourced row materialised is via a prior /close, so trust
	// it and treat as supersede.
	if !state.LastSharedRunID.Valid {
		return syncproto.DispositionSupersede, ""
	}
	if !existing.SourceRunID.Valid {
		return syncproto.DispositionConflict, "peer-sourced row has no run attribution"
	}
	sourceRun, err := r.srv.store.GetRun(ctx, existing.SourceRunID.Int64)
	if err != nil {
		return syncproto.DispositionConflict, fmt.Sprintf("source run lookup error: %v", err)
	}
	if !sourceRun.CorrelatedRunID.Valid {
		return syncproto.DispositionConflict, "source run has no correlated initiator id"
	}
	if sourceRun.CorrelatedRunID.Int64 > state.LastSharedRunID.Int64 {
		return syncproto.DispositionConflict, "peer attribution newer than the last shared watermark"
	}
	return syncproto.DispositionSupersede, ""
}

// preMoveSupersedes copies prior bytes for every supersede-bucket
// path into .squirrel-history/run-<receiverRunID>/ before /verify
// runs. This mirrors the bucket-side `rclone --backup-dir`
// invariant: the receiver owns the move (since rclone drops the
// flag for node syncs), and the move happens up front so verify
// re-hashes a clean tree.
func (r *peerSyncRouter) preMoveSupersedes(sess *peerSession) error {
	histRoot := filepath.Join(sess.volume.Path, HistoryDirName, "run-"+strconv.FormatInt(sess.receiverRunID, 10))
	for path, entry := range sess.dispositions {
		if entry.disposition != syncproto.DispositionSupersede {
			continue
		}
		srcAbs := filepath.Join(sess.volume.Path, path)
		dstAbs := filepath.Join(histRoot, path)
		if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
			return fmt.Errorf("mkdir history for %s: %w", path, err)
		}
		if err := os.Rename(srcAbs, dstAbs); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				// Prior row claims a file that isn't on disk anymore. Treat
				// as already moved (e.g. by a prior aborted sync); record
				// and proceed.
				continue
			}
			return fmt.Errorf("rename %s → %s: %w", srcAbs, dstAbs, err)
		}
	}
	return nil
}

// preStageConflicts handles every conflict-disposition path before
// rclone runs:
//
//  1. Move the prior bytes from <path> to
//     .squirrel-conflicts/run-<receiverRunID>/<path>. This frees the
//     original path so rclone can deliver the initiator's bytes
//     without `--inplace` games.
//  2. Atomically supersede the original-path row and insert the
//     conflict-path row carrying the prior blake3 + prior provenance,
//     so the losing version stays reachable by hash and by path.
//
// Disk-mv runs first; the DB mutations run together in one transaction
// via the store helper. The window between the mv and the DB commit
// is the only crash-unsafe step — an agent restart there leaves the
// file at the conflict path with both index rows in pre-call state,
// and the next /plan replans the same conflict, which is the correct
// recovery path: content stays preserved through retries.
func (r *peerSyncRouter) preStageConflicts(ctx context.Context, sess *peerSession) error {
	confSubdir := filepath.Join(ConflictsDirName, "run-"+strconv.FormatInt(sess.receiverRunID, 10))
	for _, path := range sess.conflictOrder {
		entry := sess.dispositions[path]
		preservedRel := filepath.ToSlash(filepath.Join(confSubdir, path))
		srcAbs := filepath.Join(sess.volume.Path, path)
		dstAbs := filepath.Join(sess.volume.Path, preservedRel)
		if err := os.MkdirAll(filepath.Dir(dstAbs), 0o755); err != nil {
			return fmt.Errorf("mkdir conflicts for %s: %w", path, err)
		}
		if err := os.Rename(srcAbs, dstAbs); err != nil && !errors.Is(err, os.ErrNotExist) {
			// On ErrNotExist the prior bytes are unrecoverable, but
			// the index update below still records the loser's
			// identity so a future query for the prior blake3
			// surfaces the conflict path. The next index run on the
			// receiver will mark it missing if no bytes materialise.
			return fmt.Errorf("rename %s → %s: %w", srcAbs, dstAbs, err)
		}
		conflictRow := store.FileRow{
			VolumeID:       sess.volumeID,
			Path:           preservedRel,
			Blake3:         entry.priorRow.Blake3,
			SizeBytes:      entry.priorRow.SizeBytes,
			MtimeNs:        entry.priorRow.MtimeNs,
			Status:         store.StatusPresent,
			FirstSeenRunID: sess.receiverRunID,
			LastSeenRunID:  sess.receiverRunID,
			IndexedAtNs:    store.NowNs(),
		}
		if err := r.srv.store.RecordConflictPreStage(ctx, sess.volumeID, path, conflictRow, priorProvenance(entry.priorRow)); err != nil {
			return fmt.Errorf("record conflict pre-stage for %s: %w", path, err)
		}
		entry.preservedAtPath = preservedRel
	}
	return nil
}

// priorProvenance lifts the prior row's (source_node_id, source_run_id)
// into a *store.Provenance the way Upsert expects: nil for a local
// write (both NULLs), pointer-carrying for peer-sourced rows. Either
// half being NULL is treated as "local write" — partial provenance is
// a schema-impossible state today, but degrading gracefully here keeps
// the conflict path open if a future migration ever ends up with one.
func priorProvenance(r *store.FileRow) *store.Provenance {
	if r == nil || !r.SourceNodeID.Valid || !r.SourceRunID.Valid {
		return nil
	}
	return &store.Provenance{NodeID: r.SourceNodeID.Int64, RunID: r.SourceRunID.Int64}
}

// handleVerify implements POST /v1/sync/verify.
func (r *peerSyncRouter) handleVerify(w http.ResponseWriter, req *http.Request) {
	var body syncproto.VerifyRequest
	if err := decodeJSON(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := r.lookupSession(body.ReceiverRunID)
	if sess == nil {
		writeError(w, http.StatusNotFound, "no session for receiver_run_id")
		return
	}
	resp, err := r.verifySession(sess, body.Paths)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// verifySession re-hashes every path the receiver pre-staged for
// rclone (transfer + supersede + conflict) and returns the
// reconciliation report. When scope is nil/empty the full pre-staged
// set is checked; non-empty narrows to those paths (used by
// initiator-driven retry).
func (r *peerSyncRouter) verifySession(sess *peerSession, scope []string) (syncproto.VerifyResponse, error) {
	resp := syncproto.VerifyResponse{}
	paths := scope
	if len(paths) == 0 {
		for p, e := range sess.dispositions {
			if rcloneScoped(e.disposition) {
				paths = append(paths, p)
			}
		}
	}
	for _, p := range paths {
		entry, ok := sess.dispositions[p]
		if !ok {
			resp.Unexpected = append(resp.Unexpected, p)
			continue
		}
		abs := filepath.Join(sess.volume.Path, p)
		actual, err := hashOnDisk(abs)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				resp.Missing = append(resp.Missing, p)
				continue
			}
			return syncproto.VerifyResponse{}, fmt.Errorf("hash %s: %w", p, err)
		}
		if bytesEqual(actual, entry.blake3) {
			resp.Matched = append(resp.Matched, p)
			continue
		}
		resp.Mismatched = append(resp.Mismatched, syncproto.VerifyMismatch{
			Path:        p,
			ExpectedHex: hex.EncodeToString(entry.blake3),
			ActualHex:   hex.EncodeToString(actual),
		})
	}
	return resp, nil
}

// hashOnDisk re-hashes a file with BLAKE3 — same algorithm the
// indexer uses. Streamed via io.Copy so files larger than memory work
// transparently.
func hashOnDisk(abs string) ([]byte, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// handleClose implements POST /v1/sync/close.
func (r *peerSyncRouter) handleClose(w http.ResponseWriter, req *http.Request) {
	var body syncproto.CloseRequest
	if err := decodeJSON(req, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sess := r.takeSession(body.ReceiverRunID)
	if sess == nil {
		writeError(w, http.StatusNotFound, "no session for receiver_run_id")
		return
	}
	defer r.releaseVolumeLock(sess.volumeID)

	committed, err := r.closeSession(req.Context(), sess, body.Status, body.FailedPaths)
	if err != nil {
		_ = r.srv.store.FinishRun(req.Context(), sess.receiverRunID, store.RunStatusFailed, err.Error(), int64(committed))
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, syncproto.CloseResponse{
		ReceiverRunID: sess.receiverRunID,
		Committed:     committed,
	})
}

// closeSession persists the new file rows for every path rclone was
// asked to deliver (transfer + supersede + conflict) that did not
// appear in failedPaths, advances the watermark on success, and
// finalises the receiver-side runs row. Returns the number of file
// rows the function wrote, distinct from the original plan size when
// some paths were dropped due to verify mismatch.
func (r *peerSyncRouter) closeSession(ctx context.Context, sess *peerSession, status string, failedPaths []string) (int, error) {
	skip := make(map[string]struct{}, len(failedPaths))
	for _, p := range failedPaths {
		skip[p] = struct{}{}
	}
	prov := &store.Provenance{NodeID: sess.peerNodeID, RunID: sess.receiverRunID}
	committed := 0
	for path, entry := range sess.dispositions {
		if !rcloneScoped(entry.disposition) {
			continue
		}
		if _, dropped := skip[path]; dropped {
			continue
		}
		row := store.FileRow{
			VolumeID:       sess.volumeID,
			Path:           path,
			Blake3:         entry.blake3,
			SizeBytes:      entry.size,
			MtimeNs:        entry.mtimeNs,
			Status:         store.StatusPresent,
			FirstSeenRunID: sess.receiverRunID,
			LastSeenRunID:  sess.receiverRunID,
			IndexedAtNs:    store.NowNs(),
		}
		if err := r.srv.store.Upsert(ctx, row, prov); err != nil {
			return committed, fmt.Errorf("upsert %s: %w", path, err)
		}
		committed++
	}
	if status == store.RunStatusSuccess {
		if err := r.srv.store.UpsertPeerSyncState(ctx, sess.volumeID, sess.peerNodeID, sess.correlatedRunID); err != nil {
			return committed, fmt.Errorf("advance peer_sync_state: %w", err)
		}
	}
	finishStatus := status
	if finishStatus != store.RunStatusSuccess && finishStatus != store.RunStatusPartial && finishStatus != store.RunStatusFailed {
		finishStatus = store.RunStatusPartial
	}
	if err := r.srv.store.FinishRun(ctx, sess.receiverRunID, finishStatus, "", int64(committed)); err != nil {
		return committed, fmt.Errorf("finish run: %w", err)
	}
	return committed, nil
}

// decodeJSON parses the request body with strict field handling so
// initiator typos surface as 400 rather than as a silently-ignored
// field.
func decodeJSON(req *http.Request, v any) error {
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// rcloneScoped reports whether the given disposition implies rclone
// has been asked (or will be asked) to deliver bytes at the original
// path. Used uniformly by verify (which paths to re-hash) and close
// (which paths warrant a new live row) so the two stay in lockstep.
func rcloneScoped(disposition string) bool {
	switch disposition {
	case syncproto.DispositionTransfer,
		syncproto.DispositionSupersede,
		syncproto.DispositionConflict:
		return true
	}
	return false
}

// bytesEqual is bytes.Equal in disguise; declaring it locally lets the
// package avoid the extra import in a file that does little else with
// raw bytes.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
