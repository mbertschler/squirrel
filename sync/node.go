package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

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
// passive bucket it negotiates a plan with the receiver daemon and
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
		report: &rep,
	}
	return rep, driver.run()
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

// phaseBegin opens a session with the receiver. The initiator's own
// runs row is inserted first so the (peer_node_id, correlated_run_id)
// pair lands the right way around: the receiver's id becomes our
// correlated id, not the other way around.
func (d *nodeSyncDriver) phaseBegin() error {
	peer, err := d.store.GetOrCreatePeerNode(d.ctx, d.node.Name, d.node.Endpoint.String())
	if err != nil {
		return fmt.Errorf("record peer node: %w", err)
	}
	self, err := d.store.GetSelfNode(d.ctx)
	if err != nil {
		return fmt.Errorf("look up self node: %w", err)
	}
	// correlated_run_id is filled in once we know the receiver's id.
	// Pass 0 here; SetCorrelatedRunID below stamps the real value.
	runID, err := d.store.BeginPeerSyncRun(d.ctx, d.volID, peer.ID, 0, d.node.Name)
	if err != nil {
		return fmt.Errorf("begin local run: %w", err)
	}
	d.report.RunID = runID
	resp, err := d.client.begin(d.ctx, syncproto.BeginRequest{
		Volume:            d.vol.Name,
		InitiatorNodeName: self.Name,
		InitiatorRunID:    runID,
	})
	if err != nil {
		return err
	}
	d.receiverRunID = resp.ReceiverRunID
	if err := d.store.SetCorrelatedRunID(d.ctx, runID, resp.ReceiverRunID); err != nil {
		return fmt.Errorf("stamp correlated run id: %w", err)
	}
	d.report.NodeReceiverRunID = resp.ReceiverRunID
	return nil
}

// phasePlan streams the initiator's index slice and parses the
// receiver's verdict.
func (d *nodeSyncDriver) phasePlan() (syncproto.PlanResponse, error) {
	entries, err := d.collectIndexEntries()
	if err != nil {
		return syncproto.PlanResponse{}, fmt.Errorf("collect index entries: %w", err)
	}
	return d.client.plan(d.ctx, syncproto.PlanRequest{
		ReceiverRunID: d.receiverRunID,
		Entries:       entries,
	})
}

// collectIndexEntries walks the present rows for this volume and
// builds the wire-format index slice. Only 'present' rows are
// considered — missing/superseded rows describe history, not what we
// want to push.
func (d *nodeSyncDriver) collectIndexEntries() ([]syncproto.IndexEntry, error) {
	paths, err := d.store.ListPresentPathsUnder(d.ctx, d.volID)
	if err != nil {
		return nil, err
	}
	entries := make([]syncproto.IndexEntry, 0, len(paths))
	for p := range paths {
		row, err := d.store.GetByPath(d.ctx, d.volID, p)
		if err != nil {
			return nil, fmt.Errorf("lookup %s: %w", p, err)
		}
		entries = append(entries, syncproto.IndexEntry{
			Path:      row.Path,
			Blake3Hex: hex.EncodeToString(row.Blake3),
			SizeBytes: row.SizeBytes,
			MtimeNs:   row.MtimeNs,
		})
	}
	return entries, nil
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

	result, err := d.rcl.Run(d.ctx, args...)
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
func (d *nodeSyncDriver) phaseClose() error {
	failed := failingPaths(d.report.NodeVerify)
	return d.client.close(d.ctx, syncproto.CloseRequest{
		ReceiverRunID: d.receiverRunID,
		Status:        d.report.Status,
		FailedPaths:   failed,
	})
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
		// LAN daemons.
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

func (c *nodeClient) verify(ctx context.Context, body syncproto.VerifyRequest) (syncproto.VerifyResponse, error) {
	var resp syncproto.VerifyResponse
	return resp, c.do(ctx, "/v1/sync/verify", body, &resp)
}

func (c *nodeClient) close(ctx context.Context, body syncproto.CloseRequest) error {
	return c.do(ctx, "/v1/sync/close", body, nil)
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
// they're a daemon-side accounting issue, not something we can fix
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
