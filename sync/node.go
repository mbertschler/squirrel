package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/mbertschler/squirrel/config"
	"github.com/mbertschler/squirrel/store"
	"github.com/mbertschler/squirrel/syncproto"
)

// nodeSyncRetries is the upper bound on per-run retry attempts after a
// verify mismatch, per the design (PR 3 issue): "two fast retries
// scoped to the failing paths". After this many attempts we accept
// "partial" and let the next sync re-plan from scratch.
const nodeSyncRetries = 2

// SyncNode runs one (volume, node) pair via the five-phase peer-sync
// flow described in issue #18. It mirrors Sync's shape — same Report,
// same runs-row lifecycle, same prerequisite that the source volume
// is indexed — but instead of invoking rclone directly against a
// passive bucket it negotiates a plan with the receiver agent and
// runs rclone strictly between /plan and /verify, with --backup-dir
// elided (the receiver pre-moves prior bytes itself).
func SyncNode(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, node *config.Node, opts Options) (rep Report, err error) {
	rep = Report{Volume: vol.Name, Destination: node.Name}
	if w := historyDirInSourceWarning(vol); w != "" {
		rep.Warnings = append(rep.Warnings, w)
	}
	volID, err := requireIndexedVolume(ctx, s, vol)
	if err != nil {
		return rep, err
	}
	err = runNodeSession(ctx, s, rcl, vol, volID, node, opts, &rep)
	if !opts.DryRun {
		rep.Verification = peerVerification(&rep)
	}
	// runNodeSession's deferred finishRun has committed the run's
	// terminal state by now, so the snapshot reflects this run's own row.
	// Peer-sync takes the local snapshot only — there is no ride-along to
	// peer nodes (dest=nil), and the Snapshotter no-ops on non-terminal
	// states and dry-run.
	opts.Snapshot.afterSync(ctx, &rep, vol, nil)
	return rep, err
}

// runNodeSession runs the five-phase driver and owns the deferred
// terminal-state write. It is split out of SyncNode so the deferred
// finishRun commits before the snapshot-on-sync hook runs — the snapshot
// must reflect this run's own committed row.
func runNodeSession(ctx context.Context, s *store.Store, rcl *Rclone, vol *config.Volume, volID int64, node *config.Node, opts Options, rep *Report) (err error) {
	rep.Status = store.RunStatusFailed
	defer func() {
		// Re-derive on the way out: when we never made it past Begin we
		// stay 'failed'; verifySession promotes 'success' / 'partial'.
		if rep.RunID != 0 {
			finishStatus := rep.Status
			if finishStatus == "" {
				finishStatus = store.RunStatusFailed
			}
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			fileCount := int64(len(rep.NodeVerify.Matched))
			if finishErr := s.FinishRun(ctx, rep.RunID, finishStatus, errMsg, fileCount); finishErr != nil {
				rep.FinishErr = finishErr
			}
		}
	}()

	client := newNodeClient(node)
	driver := &nodeSyncDriver{
		ctx:    ctx,
		store:  s,
		rcl:    rcl,
		vol:    vol,
		volID:  volID,
		node:   node,
		client: client,
		opts:   opts,
		report: rep,
	}
	return driver.run()
}

// nodeSyncDriver drives the five-phase initiator-side flow. The
// fields are immutable post-construction; per-phase scratch state
// lands on the Report (so the deferred FinishRun has the latest
// view) and on local variables.
type nodeSyncDriver struct {
	ctx    context.Context
	store  *store.Store
	rcl    *Rclone
	vol    *config.Volume
	volID  int64
	node   *config.Node
	client *nodeClient
	opts   Options
	report *Report
	// receiverRunID is filled in after the begin handshake; rclone +
	// verify + close reference it.
	receiverRunID int64
	// protocolVersion is the plan-exchange version negotiated at
	// /begin. Defaults to ProtocolVersionFlat so a missing field in
	// the receiver's response (older agent) keeps today's behaviour.
	protocolVersion int
	// selfNodeName caches the self-row's name for origin
	// materialisation; filled lazily by selfName.
	selfNodeName string
	// originNodeNames caches local node id → name lookups so a plan
	// full of same-origin entries resolves each origin node once.
	originNodeNames map[int64]string
	// durabilityAdvance is the present-set origin maxima captured before
	// the transfer. phaseClose advances the peer's durability vector to
	// exactly this snapshot, so a row committed between enumeration and
	// close is never claimed durable — matching the bucket, content-
	// addressed, and kopia handlers.
	durabilityAdvance []store.OriginComponent
}

// selfName returns this node's name — the identity locally-introduced
// content travels under. Cached after the first lookup.
func (d *nodeSyncDriver) selfName() (string, error) {
	if d.selfNodeName != "" {
		return d.selfNodeName, nil
	}
	self, err := d.store.GetSelfNode(d.ctx)
	if err != nil {
		return "", fmt.Errorf("look up self node: %w", err)
	}
	d.selfNodeName = self.Name
	return d.selfNodeName, nil
}

// entryOrigin materialises one row's content-origin coordinate for the
// wire. Content with a recorded origin forwards it verbatim — origin
// node id resolved to its name (names are the cross-node identity;
// local ids differ per node), run id untranslated. Locally-introduced
// content (origin NULLs, or the degraded partial-NULL state) is
// materialised as (this node's name, the content's introduction run in
// this volume).
func (d *nodeSyncDriver) entryOrigin(row store.FileRow) (string, int64, error) {
	if !row.OriginNodeID.Valid || !row.OriginRunID.Valid {
		name, err := d.selfName()
		if err != nil {
			return "", 0, err
		}
		intro, err := d.store.ContentIntroductionRunID(d.ctx, d.volID, row.ContentID)
		if err != nil {
			return "", 0, fmt.Errorf("introduction run for %s: %w", row.Path, err)
		}
		return name, intro, nil
	}
	name, err := d.originNodeName(row.OriginNodeID.Int64)
	if err != nil {
		return "", 0, err
	}
	return name, row.OriginRunID.Int64, nil
}

func (d *nodeSyncDriver) originNodeName(nodeID int64) (string, error) {
	if name, ok := d.originNodeNames[nodeID]; ok {
		return name, nil
	}
	node, err := d.store.GetNodeByID(d.ctx, nodeID)
	if err != nil {
		return "", fmt.Errorf("resolve origin node %d: %w", nodeID, err)
	}
	if d.originNodeNames == nil {
		d.originNodeNames = make(map[int64]string)
	}
	d.originNodeNames[nodeID] = node.Name
	return node.Name, nil
}

// indexEntryForRow converts one local index row to its wire form,
// attaching the materialised content origin.
func (d *nodeSyncDriver) indexEntryForRow(row store.FileRow) (syncproto.IndexEntry, error) {
	originNode, originRun, err := d.entryOrigin(row)
	if err != nil {
		return syncproto.IndexEntry{}, err
	}
	return syncproto.IndexEntry{
		Path:       row.Path,
		Blake3Hex:  hex.EncodeToString(row.Blake3),
		SizeBytes:  row.SizeBytes,
		MtimeNs:    row.MtimeNs,
		OriginNode: originNode,
		OriginRun:  originRun,
	}, nil
}

func (d *nodeSyncDriver) run() error {
	if err := d.phaseBegin(); err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	plan, err := d.phasePlan()
	if err != nil {
		return d.abortWithError("plan", err)
	}
	// Conflicts flow through the transfer path: the receiver has
	// already pre-staged each loser under .squirrel-conflicts/ and
	// the original path is empty, so rclone treats the entry like a
	// fresh transfer.
	d.report.NodeConflicts = plan.Conflicts
	d.recordAlreadyCorrect(plan)
	if !d.opts.DryRun {
		advance, err := captureDurabilityAdvance(d.ctx, d.store, d.volID)
		if err != nil {
			return d.abortWithError("capture durability advance", err)
		}
		d.durabilityAdvance = advance
	}
	if err := d.phaseTransfer(plan); err != nil {
		return d.abortWithError("transfer", err)
	}
	if err := d.phaseVerify(); err != nil {
		return d.abortWithError("verify", err)
	}
	if err := d.phaseClose(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	return nil
}

// recordAlreadyCorrect derives the count of paths the receiver already
// held correctly for the summary (F7). Under the Merkle walk only
// differing folders reach /plan, so the identical-folder files never
// appear as dispositions; already-correct is therefore present-total
// minus the paths the sync acted on (every non-already-correct
// disposition). Best-effort: a count error leaves the field zero rather
// than failing the sync over a cosmetic number.
func (d *nodeSyncDriver) recordAlreadyCorrect(plan syncproto.PlanResponse) {
	actionable := 0
	for _, disp := range plan.Dispositions {
		if disp.Disposition != syncproto.DispositionAlreadyCorrect {
			actionable++
		}
	}
	present, err := d.store.CountPresentFilesInVolume(d.ctx, d.volID)
	if err != nil {
		return
	}
	if ac := present - int64(actionable); ac > 0 {
		d.report.AlreadyCorrect = ac
	}
}

// phaseBegin opens a session with the receiver. The initiator's own
// runs row is inserted first so the (peer_node_id, correlated_run_id)
// pair lands the right way around: the receiver's id becomes our
// correlated id, not the other way around. The local insert goes
// through store.BeginSyncRunIfClear so two concurrent initiator
// invocations against the same (volume, peer) can't both proceed —
// the loser surfaces the same diagnostic the bucket path does.
func (d *nodeSyncDriver) phaseBegin() error {
	peer, err := d.store.GetOrCreatePeerNode(d.ctx, d.node.Name, d.node.Endpoint.String(), true)
	if err != nil {
		return fmt.Errorf("record peer node: %w", err)
	}
	self, err := d.store.GetSelfNode(d.ctx)
	if err != nil {
		return fmt.Errorf("look up self node: %w", err)
	}
	// correlated_run_id is filled in once we know the receiver's id.
	// Pass a zero NullInt64 here; SetCorrelatedRunID below stamps the
	// real value after /v1/sync/begin returns.
	runID, blocker, err := d.store.BeginSyncRunIfClear(d.ctx, store.SyncRunSpec{
		VolumeID:    d.volID,
		Destination: d.node.Name,
		PeerNodeID:  sql.NullInt64{Int64: peer.ID, Valid: true},
		Shallow:     d.opts.Shallow,
	})
	if err != nil {
		return fmt.Errorf("begin local run: %w", err)
	}
	if blocker != nil {
		return alreadyRunningErr(d.vol.Name, d.node.Name, blocker)
	}
	d.report.RunID = runID
	if d.opts.OnRunID != nil && runID != 0 {
		d.opts.OnRunID(runID)
	}
	resp, err := d.client.begin(d.ctx, syncproto.BeginRequest{
		Volume:            d.vol.Name,
		InitiatorNodeName: self.Name,
		InitiatorRunID:    runID,
		DedupStrategy:     d.node.DedupStrategy,
		ProtocolVersion:   syncproto.ProtocolVersionMerkleWalk,
	})
	if err != nil {
		return err
	}
	d.receiverRunID = resp.ReceiverRunID
	if resp.ProtocolVersion <= 0 {
		d.protocolVersion = syncproto.ProtocolVersionFlat
	} else {
		d.protocolVersion = resp.ProtocolVersion
	}
	if err := d.store.SetCorrelatedRunID(d.ctx, runID, resp.ReceiverRunID); err != nil {
		return fmt.Errorf("stamp correlated run id: %w", err)
	}
	d.report.NodeReceiverRunID = resp.ReceiverRunID
	d.report.NodePendingWarnings = resp.PendingWarnings
	return nil
}

// phasePlan streams the initiator's index slice and parses the
// receiver's verdict. Under ProtocolVersionMerkleWalk the slice is
// scoped to files in folders the walk identified as differing; under
// ProtocolVersionFlat it is every present file in the volume.
func (d *nodeSyncDriver) phasePlan() (syncproto.PlanResponse, error) {
	entries, err := d.collectPlanEntries()
	if err != nil {
		return syncproto.PlanResponse{}, fmt.Errorf("collect index entries: %w", err)
	}
	return d.client.plan(d.ctx, syncproto.PlanRequest{
		ReceiverRunID: d.receiverRunID,
		Entries:       entries,
	})
}

// collectPlanEntries chooses between the v2 walk and the v1 flat
// enumeration based on the negotiated protocol version, so phasePlan
// itself stays a thin wrapper around the wire call.
func (d *nodeSyncDriver) collectPlanEntries() ([]syncproto.IndexEntry, error) {
	if d.protocolVersion >= syncproto.ProtocolVersionMerkleWalk {
		return d.collectEntriesViaWalk()
	}
	return d.collectIndexEntries()
}

// collectEntriesViaWalk drives the Merkle walk: it asks the receiver
// for folder digests level by level, identifies which folders differ
// (so their files need /plan classification), and returns just those
// folders' files. The breadth-first order means one HTTP round-trip
// per tree depth regardless of how many folders share that depth.
// Folder paths reserved for sync internals (.squirrel-history/,
// .squirrel-conflicts/) are filtered before queueing so the walk
// never crosses into them.
func (d *nodeSyncDriver) collectEntriesViaWalk() ([]syncproto.IndexEntry, error) {
	queue := []string{""}
	var differingFolderIDs []int64
	for len(queue) > 0 {
		resp, err := d.client.planFolders(d.ctx, syncproto.PlanFoldersRequest{
			ReceiverRunID: d.receiverRunID,
			Paths:         queue,
		})
		if err != nil {
			return nil, fmt.Errorf("plan-folders walk: %w", err)
		}
		next, leaves, err := d.processWalkResponse(resp)
		if err != nil {
			return nil, err
		}
		differingFolderIDs = append(differingFolderIDs, leaves...)
		queue = next
	}
	return d.entriesFromFolders(differingFolderIDs)
}

// processWalkResponse zips the receiver's reply with the initiator's
// local view of each folder. It returns (a) the next-level paths to
// descend into and (b) the local folder IDs whose direct files must
// be sent to /plan. A folder where deep_blake3 matches contributes
// neither: the subtree is identical on both sides.
func (d *nodeSyncDriver) processWalkResponse(resp syncproto.PlanFoldersResponse) ([]string, []int64, error) {
	var next []string
	var differingIDs []int64
	for _, fd := range resp.Folders {
		local, err := d.store.GetFolderByPath(d.ctx, d.volID, fd.Path)
		if store.IsNotFound(err) {
			// Initiator queued this only when its own walk surfaced it,
			// so a missing local row would mean the DB was mutated
			// mid-sync. Skip gracefully — the divergence will resurface
			// on the next sync.
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("local folder %q: %w", fd.Path, err)
		}
		// Treat an empty digest on either side as "unknown" and force
		// descent — comparing two empty hex strings would otherwise
		// silently skip a real divergence (e.g. an as-yet-unhashed
		// folder seeded by the migration's second pass).
		localDeepHex := hex.EncodeToString(local.DeepBlake3)
		if fd.Present && localDeepHex != "" && fd.DeepHex != "" && fd.DeepHex == localDeepHex {
			continue
		}
		localShallowHex := hex.EncodeToString(local.ShallowBlake3)
		shallowDiffers := !fd.Present || localShallowHex == "" || fd.ShallowHex == "" || fd.ShallowHex != localShallowHex
		if shallowDiffers && !isReservedFolderPath(fd.Path) {
			differingIDs = append(differingIDs, local.ID)
		}
		// When the receiver has no folder at this path, the entire
		// local subtree is initiator-only — every descendant folder's
		// files belong in /plan. Collecting them locally saves N
		// round-trips that would just confirm "still absent" against
		// the receiver.
		if !fd.Present {
			descIDs, err := d.collectInitiatorOnlySubtree(local.ID)
			if err != nil {
				return nil, nil, err
			}
			differingIDs = append(differingIDs, descIDs...)
			continue
		}
		childNext, err := d.queueChildrenToDescend(local, fd)
		if err != nil {
			return nil, nil, err
		}
		next = append(next, childNext...)
	}
	return next, differingIDs, nil
}

// collectInitiatorOnlySubtree returns every descendant folder ID
// under parentID whose files should be sent to /plan as a single
// initiator-only subtree. Used when the receiver reported the parent
// as absent: rather than walking each level just to confirm "still
// absent", we resolve the whole subtree locally in O(subtree-size)
// store queries with no network round-trips. Reserved-folder
// descendants are filtered so .squirrel-history / .squirrel-conflicts
// subtrees never end up in /plan.
func (d *nodeSyncDriver) collectInitiatorOnlySubtree(parentID int64) ([]int64, error) {
	var out []int64
	queue := []int64{parentID}
	for len(queue) > 0 {
		var next []int64
		for _, id := range queue {
			kids, err := d.store.ListChildFolders(d.ctx, id)
			if err != nil {
				return nil, fmt.Errorf("list children of %d: %w", id, err)
			}
			for _, k := range kids {
				if isReservedFolderPath(k.Path) {
					continue
				}
				out = append(out, k.ID)
				next = append(next, k.ID)
			}
		}
		queue = next
	}
	return out, nil
}

// queueChildrenToDescend returns the local children of one folder
// whose deep_blake3 differs from the receiver's (or who exist only on
// the initiator). Reserved-name children are filtered so the walk
// never enters .squirrel-history/ or .squirrel-conflicts/. Receiver-
// only children are intentionally ignored: those subtrees hold files
// the initiator doesn't have, which the initiator can't push.
func (d *nodeSyncDriver) queueChildrenToDescend(local store.Folder, fd syncproto.FolderDigest) ([]string, error) {
	localKids, err := d.store.ListChildFolders(d.ctx, local.ID)
	if err != nil {
		return nil, fmt.Errorf("list child folders of %q: %w", local.Path, err)
	}
	recvDeepByName := make(map[string]string, len(fd.Children))
	for _, c := range fd.Children {
		recvDeepByName[c.Name] = c.DeepHex
	}
	var out []string
	for _, k := range localKids {
		if isReservedFolderPath(k.Path) {
			continue
		}
		localDeep := hex.EncodeToString(k.DeepBlake3)
		recvDeep, hasRecv := recvDeepByName[k.Name()]
		if hasRecv && localDeep != "" && recvDeep != "" && recvDeep == localDeep {
			continue
		}
		out = append(out, k.Path)
	}
	return out, nil
}

// entriesFromFolders flattens the per-folder file rows the walk
// surfaced into a /plan-shaped slice. Reserved-path rows are filtered
// here too (the walk already skips reserved folder paths, but a stray
// file directly at the reserved-name boundary still needs guarding).
func (d *nodeSyncDriver) entriesFromFolders(folderIDs []int64) ([]syncproto.IndexEntry, error) {
	var entries []syncproto.IndexEntry
	for _, fid := range folderIDs {
		files, err := d.store.ListPresentFilesInFolder(d.ctx, fid)
		if err != nil {
			return nil, fmt.Errorf("list files in folder %d: %w", fid, err)
		}
		for _, row := range files {
			if isReservedSyncPath(row.Path) {
				continue
			}
			entry, err := d.indexEntryForRow(row)
			if err != nil {
				return nil, err
			}
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// collectIndexEntries walks the present rows for this volume and
// builds the wire-format index slice. Only 'present' rows are
// considered — missing/superseded rows describe history, not what we
// want to push. Rows under reserved sync directories
// (.squirrel-history/, .squirrel-conflicts/) are filtered out so a
// node that received conflict preservation in a prior sync doesn't
// re-publish the loser's content to peers when it later acts as an
// initiator. The local DB row stays put — `squirrel query` against
// the prior blake3 still resolves — but the path is not on the wire.
func (d *nodeSyncDriver) collectIndexEntries() ([]syncproto.IndexEntry, error) {
	paths, err := d.store.ListPresentPathsUnder(d.ctx, d.volID)
	if err != nil {
		return nil, err
	}
	entries := make([]syncproto.IndexEntry, 0, len(paths))
	for p := range paths {
		if isReservedSyncPath(p) {
			continue
		}
		row, err := d.store.GetByPath(d.ctx, d.volID, p)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", p, err)
		}
		entry, err := d.indexEntryForRow(row)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// isReservedSyncPath reports whether p lives under one of the
// receiver-owned reserved subtrees. Kept here rather than in the
// store because the reserved-ness is a sync-layer concern: the DB
// happily stores rows under any path. Matches *files* under those
// subtrees — a file at the bare reserved-dir name is impossible.
// .squirrel-restore-history is local-side only (created by
// `restore --in-place`); the receiver doesn't write into it but a
// peer's index can contain rows pointing there, and those must not
// propagate via node sync.
func isReservedSyncPath(p string) bool {
	return strings.HasPrefix(p, HistoryDirName+"/") ||
		strings.HasPrefix(p, ConflictsDirName+"/") ||
		strings.HasPrefix(p, RestoreHistoryDirName+"/") ||
		strings.HasPrefix(p, IndexDirName+"/")
}

// isReservedFolderPath is the folder-path variant of
// isReservedSyncPath: a folder whose path is *exactly* the reserved
// directory name (e.g. ".squirrel-history") also qualifies. Folder
// paths carry no trailing slash, so the file-path predicate would
// miss the exact bare-name match and queue the reserved folder into
// the Merkle walk — which the receiver's validateFolderPath then
// rejects, aborting the whole walk.
func isReservedFolderPath(p string) bool {
	return p == HistoryDirName || p == ConflictsDirName || p == RestoreHistoryDirName ||
		p == IndexDirName || isReservedSyncPath(p)
}

// phaseTransfer invokes rclone exactly once over the transfer +
// supersede + conflict paths the plan returned. Re-uses the existing
// Rclone wrapper but with a different argv: no --backup-dir, an
// --files-from containing the in-scope paths, and no .squirrel-history
// filter (the receiver doesn't share that namespace through HTTP).
// Conflict paths are in scope because the receiver moved the prior
// bytes aside in pre-stage; rclone just delivers the initiator's
// version to the now-empty original path.
func (d *nodeSyncDriver) phaseTransfer(plan syncproto.PlanResponse) error {
	transferPaths := pathsInScope(plan)
	if len(transferPaths) == 0 {
		return nil
	}
	return d.invokeRclone(transferPaths)
}

// invokeRclone runs rclone copy with --files-from over the supplied
// relative paths. The destination URI is constructed from the node's
// Path field (the rclone target prefix) joined with the volume name.
// For node syncs the source argument is the volume's absolute path,
// just as for bucket syncs.
func (d *nodeSyncDriver) invokeRclone(transferPaths []string) error {
	listFile, cleanup, err := writeFilesFrom(transferPaths)
	if err != nil {
		return fmt.Errorf("write files-from list: %w", err)
	}
	defer cleanup()

	args := []string{
		"copy",
		"--files-from-raw", listFile,
	}
	if !d.opts.Shallow {
		args = append(args, "--checksum", "--hash", "blake3")
	}
	if d.opts.DryRun {
		args = append(args, "--dry-run")
	}
	args = append(args, withTrailingSlash(d.vol.Path), nodeRcloneDest(d.node, d.vol.Name))

	result, err := d.rcl.RunWithProgress(d.ctx, d.opts.Progress, args...)
	d.report.RcloneResult = result
	if err != nil {
		return fmt.Errorf("rclone: %w", err)
	}
	if result.Errors > 0 || result.FatalError {
		return fmt.Errorf("rclone reported %d errors", result.Errors)
	}
	return nil
}

// phaseVerify drives the verify endpoint plus up to nodeSyncRetries
// retry rounds scoped to the failing subset. Success at the first
// pass yields RunStatusSuccess; success after retries with no
// leftover delta also yields RunStatusSuccess; an unrecoverable
// remainder yields RunStatusPartial.
func (d *nodeSyncDriver) phaseVerify() error {
	resp, err := d.client.verify(d.ctx, syncproto.VerifyRequest{
		ReceiverRunID: d.receiverRunID,
	})
	if err != nil {
		return err
	}
	d.report.NodeVerify = resp

	for attempt := 0; attempt < nodeSyncRetries && verifyHasDelta(resp); attempt++ {
		failing := failingPaths(resp)
		if len(failing) == 0 {
			break
		}
		if err := d.invokeRclone(failing); err != nil {
			return fmt.Errorf("retry %d transfer: %w", attempt+1, err)
		}
		resp, err = d.client.verify(d.ctx, syncproto.VerifyRequest{
			ReceiverRunID: d.receiverRunID,
			Paths:         failing,
		})
		if err != nil {
			return err
		}
		d.report.NodeVerify = resp
	}
	if verifyHasDelta(resp) {
		d.report.Status = store.RunStatusPartial
		return nil
	}
	d.report.Status = store.RunStatusSuccess
	return nil
}

// phaseClose tells the receiver to commit. We always send the final
// verify report's failing paths as 'failed_paths' so the receiver
// skips them on commit — they'll be picked up by the next sync's
// /plan when their on-disk content reappears.
//
// On a verified successful close (status success acknowledged by the
// receiver; never dry-run) the initiator records the durability
// consequence: the peer is a destination in the flat target namespace,
// so the vector advances over the present-set origin maxima captured
// before the transfer (tagged peer-blake3 — the receiver re-hashed every
// delivered path). Pinning to that snapshot keeps a row committed between
// enumeration and close from being claimed durable, matching the other
// handlers. A failed advance fails the run — the bytes are on the peer
// but the evidence isn't recorded, and the next sync re-plans (everything
// already-correct) and re-advances cheaply. The durability pull that
// follows is metadata-only and merely warns on failure.
func (d *nodeSyncDriver) phaseClose() error {
	failed := failingPaths(d.report.NodeVerify)
	err := d.client.close(d.ctx, syncproto.CloseRequest{
		ReceiverRunID: d.receiverRunID,
		Status:        d.report.Status,
		FailedPaths:   failed,
	})
	if err != nil {
		return err
	}
	if d.report.Status != store.RunStatusSuccess || d.opts.DryRun {
		return nil
	}
	if err := d.store.AdvanceDestinationVectorTo(d.ctx, d.volID, d.node.Name, store.VerifyMethodPeer, d.durabilityAdvance); err != nil {
		return fmt.Errorf("advance destination vector for %s: %w", d.node.Name, err)
	}
	d.pullPeerDurability()
	return nil
}

// pullPeerDurability fetches the peer's destination vectors and merges
// them into the local store. Failures, refused rewinds, and components
// dropped for unconfigured destinations surface as report warnings
// rather than failing the run: the sync itself succeeded, and the pull
// can be retried any time via the standalone `peer-sync pull-durability`
// command.
func (d *nodeSyncDriver) pullPeerDurability() {
	rep, err := pullDurability(d.ctx, d.store, d.client, d.vol.Name, d.volID, d.node.Name, acceptedDestinations(d.vol), false)
	d.report.DurabilityPull = rep
	if err != nil {
		d.report.Warnings = append(d.report.Warnings,
			fmt.Sprintf("durability pull from %s: %v", d.node.Name, err))
		return
	}
	for _, rw := range rep.Rewinds {
		d.report.Warnings = append(d.report.Warnings,
			fmt.Sprintf("durability pull from %s refused rewind: %s", d.node.Name, rw))
	}
	if rep.Dropped > 0 {
		d.report.Warnings = append(d.report.Warnings,
			fmt.Sprintf("durability pull from %s dropped %d entr%s for unconfigured destinations (e.g. %s)",
				d.node.Name, rep.Dropped, plural(rep.Dropped, "y", "ies"), dropSample(rep.Drops)))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// dropSample renders the sampled destinations from a drop list as a
// compact, deduplicated, comma-separated string for one summary line.
func dropSample(drops []DurabilityDrop) string {
	seen := make(map[string]struct{}, len(drops))
	var names []string
	for _, d := range drops {
		if _, ok := seen[d.Destination]; ok {
			continue
		}
		seen[d.Destination] = struct{}{}
		names = append(names, d.Destination)
	}
	return strings.Join(names, ", ")
}

func (d *nodeSyncDriver) abortWithError(phase string, err error) error {
	d.report.Status = store.RunStatusFailed
	if d.receiverRunID != 0 {
		_ = d.client.close(d.ctx, syncproto.CloseRequest{
			ReceiverRunID: d.receiverRunID,
			Status:        store.RunStatusFailed,
		})
	}
	return fmt.Errorf("%s: %w", phase, err)
}

// nodeClient wraps the HTTP transport against one peer node. The
// http.Client carries the optional TLS pin so the verifier sees the
// node-specific fingerprint without leaking it into request paths.
type nodeClient struct {
	node   *config.Node
	client *http.Client
}

func newNodeClient(n *config.Node) *nodeClient {
	cli := &http.Client{Transport: buildTransport(n)}
	return &nodeClient{node: n, client: cli}
}

// buildTransport returns an http.Transport tuned for the node. When
// the node carries a cert fingerprint, the transport's
// VerifyConnection callback enforces it (the standard chain
// verification is disabled in that path because self-signed certs
// are normal — pinning is the trust anchor).
func buildTransport(n *config.Node) *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if n.CertFingerprint == "" {
		return tr
	}
	expected, _ := hex.DecodeString(stripFingerprintPrefix(n.CertFingerprint))
	tr.TLSClientConfig = &tls.Config{
		// We do our own verification below; skip the default chain
		// check because pin-trust is the contract for self-signed
		// LAN agents.
		InsecureSkipVerify: true, // #nosec G402 -- pinned via VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no peer certificate presented")
			}
			actual := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !bytes.Equal(actual[:], expected) {
				return fmt.Errorf("peer cert fingerprint mismatch: got %s, want %s",
					hex.EncodeToString(actual[:]), n.CertFingerprint)
			}
			return nil
		},
	}
	return tr
}

func stripFingerprintPrefix(fp string) string {
	const prefix = "sha256:"
	if len(fp) > len(prefix) && fp[:len(prefix)] == prefix {
		return fp[len(prefix):]
	}
	return fp
}

func (c *nodeClient) begin(ctx context.Context, body syncproto.BeginRequest) (syncproto.BeginResponse, error) {
	var resp syncproto.BeginResponse
	return resp, c.do(ctx, "/v1/sync/begin", body, &resp)
}

func (c *nodeClient) plan(ctx context.Context, body syncproto.PlanRequest) (syncproto.PlanResponse, error) {
	var resp syncproto.PlanResponse
	return resp, c.do(ctx, "/v1/sync/plan", body, &resp)
}

func (c *nodeClient) planFolders(ctx context.Context, body syncproto.PlanFoldersRequest) (syncproto.PlanFoldersResponse, error) {
	var resp syncproto.PlanFoldersResponse
	return resp, c.do(ctx, "/v1/sync/plan-folders", body, &resp)
}

func (c *nodeClient) verify(ctx context.Context, body syncproto.VerifyRequest) (syncproto.VerifyResponse, error) {
	var resp syncproto.VerifyResponse
	return resp, c.do(ctx, "/v1/sync/verify", body, &resp)
}

func (c *nodeClient) close(ctx context.Context, body syncproto.CloseRequest) error {
	return c.do(ctx, "/v1/sync/close", body, nil)
}

func (c *nodeClient) durability(ctx context.Context, body syncproto.DurabilityRequest) (syncproto.DurabilityResponse, error) {
	var resp syncproto.DurabilityResponse
	return resp, c.do(ctx, "/v1/sync/durability", body, &resp)
}

// do is the shared "POST JSON, decode JSON" implementation. The URL
// is built by joining the configured endpoint's path with urlPath
// (rather than concatenating raw strings, per CLAUDE.md) — a node
// reachable at https://nas.local:8443/squirrel/ would dispatch to
// https://nas.local:8443/squirrel/v1/sync/begin without leaking
// either the prefix or the action name into request bodies.
// Non-2xx responses surface as errors carrying the receiver's
// `error` field when present.
func (c *nodeClient) do(ctx context.Context, urlPath string, body, out any) error {
	full := *c.node.Endpoint
	full.Path = path.Join(c.node.Endpoint.Path, urlPath)
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s: %w", urlPath, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full.String(), bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("new request %s: %w", urlPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.node.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("post %s: %w", urlPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		bodyBytes, _ := io.ReadAll(resp.Body)
		var errBody syncproto.ErrorResponse
		_ = json.Unmarshal(bodyBytes, &errBody)
		if errBody.Error != "" {
			return fmt.Errorf("%s: %s (%d)", urlPath, errBody.Error, resp.StatusCode)
		}
		return fmt.Errorf("%s: status %d", urlPath, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pathsInScope returns the relative paths from the plan whose
// disposition needs rclone to deliver bytes — transfer, supersede,
// conflict. Already-correct paths are skipped (no bytes need move).
// Copy-from-existing paths are skipped too: the receiver has already
// materialised them locally during pre-stage, and the initiator's
// verify step picks them up via the receiver-side scope (which
// includes them) rather than via a redundant rclone pass.
func pathsInScope(plan syncproto.PlanResponse) []string {
	out := make([]string, 0, len(plan.Dispositions))
	for _, d := range plan.Dispositions {
		switch d.Disposition {
		case syncproto.DispositionTransfer,
			syncproto.DispositionSupersede,
			syncproto.DispositionConflict:
			out = append(out, d.Path)
		}
	}
	return out
}

// failingPaths flattens the verify response's mismatched + missing
// lists into a single retry scope. Unexpected paths aren't retried —
// they're an agent-side accounting issue, not something we can fix
// from the initiator.
func failingPaths(r syncproto.VerifyResponse) []string {
	out := make([]string, 0, len(r.Mismatched)+len(r.Missing))
	for _, m := range r.Mismatched {
		out = append(out, m.Path)
	}
	out = append(out, r.Missing...)
	return out
}

func verifyHasDelta(r syncproto.VerifyResponse) bool {
	return len(r.Mismatched) > 0 || len(r.Missing) > 0
}

// nodeRcloneDest builds the rclone destination URI for the given
// volume under a node. The node's Path field is treated as an
// rclone-style prefix: absolute filesystem path, or "remote:path".
// We never pass `.squirrel-history` through here — the receiver owns
// that directory.
func nodeRcloneDest(node *config.Node, volumeName string) string {
	joined := path.Join(node.Path, volumeName)
	if len(joined) > 0 && joined[len(joined)-1] != '/' {
		joined += "/"
	}
	return joined
}

// writeFilesFrom writes the given relative paths to a temp file and
// returns the path. The caller invokes cleanup() to remove the file
// once rclone has been launched (rclone reads the list in full at
// startup so deferring is safe).
func writeFilesFrom(paths []string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "squirrel-files-from-")
	if err != nil {
		return "", func() {}, err
	}
	listPath := filepath.Join(dir, "list")
	f, err := os.Create(listPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, err
	}
	for _, p := range paths {
		if _, err := f.WriteString(p + "\n"); err != nil {
			_ = f.Close()
			_ = os.RemoveAll(dir)
			return "", func() {}, err
		}
	}
	if err := f.Close(); err != nil {
		_ = os.RemoveAll(dir)
		return "", func() {}, err
	}
	return listPath, func() { _ = os.RemoveAll(dir) }, nil
}
