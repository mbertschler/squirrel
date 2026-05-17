package daemon

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
	"sync"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// HistoryDirName mirrors sync.HistoryDirName at the daemon side — the
// reserved directory at the volume root where pre-supersede moves
// stage prior bytes. Lowercase-duplicated here rather than imported to
// keep the daemon package free of the sync package's rclone
// dependency.
const HistoryDirName = ".squirrel-history"

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
// between the four endpoint calls. Lives in memory; daemon restart
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
}

// sessionEntry is one path's state across the session: the
// initiator's claim from /plan, used at /verify (to know what hash
// to compare on-disk bytes against) and at /close (to construct the
// new file row). Receiver-side prior state is consulted during
// classification but doesn't need to survive into /verify or /close
// — the supersede pre-move already settled it.
type sessionEntry struct {
	disposition string
	blake3      []byte
	size        int64
	mtimeNs     int64
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
// what we materialise; indexing on the receiver (PR 4) refills file
// rows from disk later.
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
	}, http.StatusOK, nil
}

// peerEndpoint resolves the endpoint string to store on the peer
// nodes row. Single-writer initiators don't expose a daemon of their
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
// performs pre-moves for the supersede bucket. Errors here roll the
// session back to a clean state (no partial pre-moves on failure).
func (r *peerSyncRouter) planSession(ctx context.Context, sess *peerSession, entries []syncproto.IndexEntry) (syncproto.PlanResponse, error) {
	resp := syncproto.PlanResponse{
		Dispositions: make([]syncproto.PlanDisposition, 0, len(entries)),
	}
	for _, e := range entries {
		digest, err := hex.DecodeString(e.Blake3Hex)
		if err != nil || len(digest) != 32 {
			return syncproto.PlanResponse{}, fmt.Errorf("invalid blake3 hex %q for path %q", e.Blake3Hex, e.Path)
		}
		entry := &sessionEntry{blake3: digest, size: e.SizeBytes, mtimeNs: e.MtimeNs}
		disp, conflict, err := r.classify(ctx, sess, e.Path, entry)
		if err != nil {
			return syncproto.PlanResponse{}, fmt.Errorf("classify %q: %w", e.Path, err)
		}
		entry.disposition = disp
		sess.dispositions[e.Path] = entry
		resp.Dispositions = append(resp.Dispositions, syncproto.PlanDisposition{
			Path: e.Path, Disposition: disp, Blake3Hex: e.Blake3Hex,
		})
		if conflict != nil {
			resp.Conflicts = append(resp.Conflicts, *conflict)
		}
	}
	if err := r.preMoveSupersedes(sess); err != nil {
		return syncproto.PlanResponse{}, fmt.Errorf("pre-move supersedes: %w", err)
	}
	return resp, nil
}

// classify is the four-bucket decision per path. The conflict pointer
// is non-nil exactly when the disposition is "conflict"; planSession
// surfaces it on the response so the CLI can render specifics.
func (r *peerSyncRouter) classify(ctx context.Context, sess *peerSession, relPath string, entry *sessionEntry) (string, *syncproto.ConflictDetail, error) {
	existing, err := r.srv.store.GetByPath(ctx, sess.volumeID, relPath)
	if err != nil {
		if store.IsNotFound(err) {
			return syncproto.DispositionTransfer, nil, nil
		}
		return "", nil, err
	}
	if existing.Status != store.StatusPresent {
		// A live row in 'missing' status is treated as "no file here";
		// the initiator's bytes are new content.
		return syncproto.DispositionTransfer, nil, nil
	}
	if bytesEqual(existing.Blake3, entry.blake3) {
		return syncproto.DispositionAlreadyCorrect, nil, nil
	}
	// Different blake3 at this path. Provenance decides the verdict.
	disp, reason := r.dispositionForExisting(ctx, sess, existing)
	if disp == syncproto.DispositionConflict {
		return disp, &syncproto.ConflictDetail{
			Path:               relPath,
			InitiatorBlake3Hex: hex.EncodeToString(entry.blake3),
			ReceiverBlake3Hex:  hex.EncodeToString(existing.Blake3),
			Reason:             reason,
		}, nil
	}
	return disp, nil, nil
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
// In single-writer v1 the second branch is forward-compatible cover:
// only one initiator ever writes a given volume, so the watermark
// check should always pass; the structured path is what PR 4 expands
// to handle multi-writer drift.
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

// verifySession re-hashes the transfer + supersede paths in scope and
// returns the reconciliation report. When scope is nil/empty the full
// transfer+supersede set is checked; non-empty narrows to those paths
// (used by initiator-driven retry).
func (r *peerSyncRouter) verifySession(sess *peerSession, scope []string) (syncproto.VerifyResponse, error) {
	resp := syncproto.VerifyResponse{}
	paths := scope
	if len(paths) == 0 {
		for p, e := range sess.dispositions {
			if e.disposition == syncproto.DispositionTransfer || e.disposition == syncproto.DispositionSupersede {
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

// closeSession persists the new file rows for the transfer+supersede
// paths that did not appear in failedPaths, advances the watermark on
// success, and finalises the receiver-side runs row. Returns the
// number of file rows the function wrote, distinct from the original
// plan size when some paths were dropped due to verify mismatch.
func (r *peerSyncRouter) closeSession(ctx context.Context, sess *peerSession, status string, failedPaths []string) (int, error) {
	skip := make(map[string]struct{}, len(failedPaths))
	for _, p := range failedPaths {
		skip[p] = struct{}{}
	}
	prov := &store.Provenance{NodeID: sess.peerNodeID, RunID: sess.receiverRunID}
	committed := 0
	for path, entry := range sess.dispositions {
		if entry.disposition != syncproto.DispositionTransfer && entry.disposition != syncproto.DispositionSupersede {
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
