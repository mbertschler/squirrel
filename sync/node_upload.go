package sync

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/syncproto"
)

// plannedContent is the content this node declared for one path at /plan.
type plannedContent struct {
	hashHex string
	size    int64
}

func plannedContentByPath(entries []syncproto.IndexEntry) map[string]plannedContent {
	out := make(map[string]plannedContent, len(entries))
	for _, e := range entries {
		out[e.Path] = plannedContent{hashHex: e.Blake3Hex, size: e.SizeBytes}
	}
	return out
}

// errUploadSource marks an upload that failed reading its source file on
// this machine.
var errUploadSource = errors.New("read upload source")

// errUploadStalled marks an upload the receiver stopped accepting bytes
// for, or stopped answering, for longer than the stall timeout.
var errUploadStalled = errors.New("upload stalled")

// peerRejection is a receiver's refusal of one content object: bytes that
// did not match their digest or size, or content no path in the session
// awaits.
type peerRejection struct {
	msg string
}

func (e *peerRejection) Error() string { return e.msg }

// uploadJob is one content object to send: the paths awaiting it, the
// first of which is read as the source.
type uploadJob struct {
	rels []string
	plannedContent
}

// uploadPaths delivers the content behind the given volume-relative
// paths. Paths sharing a BLAKE3 collapse to one upload — the receiver
// materialises every path in the session that wants those bytes — so a
// volume with duplicate files transfers each distinct content once.
//
// A failure confined to one content object (its source vanished or
// changed, or the receiver refused its bytes) is recorded and the rest
// carry on: /verify reports its paths, the retry round re-sends them, and
// a path that still fails leaves the run partial with everything else
// committed. Any other failure means the connection or the receiver is
// unwell, so it ends the phase; the receiver keeps what already landed.
//
// Dry-run stops before sending: the plan already told the operator what
// would move.
func (d *nodeSyncDriver) uploadPaths(paths []string) error {
	jobs, err := d.uploadJobs(paths)
	if err != nil {
		return err
	}
	if d.opts.DryRun {
		for _, j := range jobs {
			d.report.RcloneResult.Transferred++
			d.report.RcloneResult.Bytes += j.size
		}
		return nil
	}
	for _, job := range jobs {
		err := d.uploadOne(job)
		switch {
		case err == nil:
			d.report.RcloneResult.Transferred++
			d.report.RcloneResult.Bytes += job.size
			d.emitProgress(len(jobs))
		case confinedToContent(err):
			d.noteUploadFailure(job, err)
		default:
			return fmt.Errorf("upload %s: %w", job.rels[0], err)
		}
	}
	return nil
}

func (d *nodeSyncDriver) uploadJobs(paths []string) ([]uploadJob, error) {
	byHash := make(map[string]*uploadJob, len(paths))
	for _, rel := range slices.Sorted(slices.Values(paths)) {
		content, ok := d.planned[rel]
		if !ok {
			return nil, fmt.Errorf("no planned content for %q", rel)
		}
		if job, seen := byHash[content.hashHex]; seen {
			job.rels = append(job.rels, rel)
			continue
		}
		byHash[content.hashHex] = &uploadJob{rels: []string{rel}, plannedContent: content}
	}
	jobs := make([]uploadJob, 0, len(byHash))
	for _, hashHex := range slices.Sorted(maps.Keys(byHash)) {
		jobs = append(jobs, *byHash[hashHex])
	}
	return jobs, nil
}

// uploadOne streams one content object to the receiver, hashing exactly
// the bytes sent. It reads the planned size and no more, so a file
// appended to since it was indexed still delivers its indexed prefix,
// while one rewritten since fails with errContentDrift naming the file.
// The receiver checks the same digest independently and is the authority.
func (d *nodeSyncDriver) uploadOne(job uploadJob) error {
	f, err := d.openUploadSource(job)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	src := &io.LimitedReader{R: f, N: job.size}
	hasher := blake3.New()
	putErr := d.client.putContent(d.ctx, d.receiverRunID, job.hashHex, io.TeeReader(src, hasher), job.size)
	var rejected *peerRejection
	if src.N == 0 && (putErr == nil || errors.As(putErr, &rejected)) {
		if sent := hex.EncodeToString(hasher.Sum(nil)); sent != job.hashHex {
			return fmt.Errorf("%w: %s hashed to %s while sending, indexed as %s — run `squirrel index %s` and sync again",
				errContentDrift, job.rels[0], sent, job.hashHex, d.vol.Name)
		}
	}
	return putErr
}

func (d *nodeSyncDriver) openUploadSource(job uploadJob) (*os.File, error) {
	f, err := os.Open(filepath.Join(d.vol.Path, job.rels[0]))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUploadSource, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %w", errUploadSource, err)
	}
	if info.Size() < job.size {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is %d bytes, indexed as %d — run `squirrel index %s` and sync again",
			errContentDrift, job.rels[0], info.Size(), job.size, d.vol.Name)
	}
	return f, nil
}

func confinedToContent(err error) bool {
	var rejected *peerRejection
	return errors.Is(err, errUploadSource) || errors.Is(err, errContentDrift) || errors.As(err, &rejected)
}

func (d *nodeSyncDriver) noteUploadFailure(job uploadJob, err error) {
	if d.uploadFailures == nil {
		d.uploadFailures = make(map[string]string)
	}
	for _, rel := range job.rels {
		d.uploadFailures[rel] = err.Error()
	}
}

// recordFailedPaths lists the paths the final /verify still reports
// failing on the report, each with the upload error that explains it
// when there was one.
func (d *nodeSyncDriver) recordFailedPaths(resp syncproto.VerifyResponse) {
	res := &d.report.RcloneResult
	for _, rel := range failingPaths(resp) {
		res.Errors++
		if int64(len(res.FailedFiles)) >= maxFailedFiles {
			continue
		}
		msg, ok := d.uploadFailures[rel]
		if !ok {
			msg = "content did not verify on " + d.node.Name
		}
		res.FailedFiles = append(res.FailedFiles, FailedFile{Object: rel, Message: msg})
	}
}

// emitProgress reports upload advance to the optional progress sink,
// reusing the counters already folded into the report so the CLI and the
// desktop see the same numbers the run row will carry.
func (d *nodeSyncDriver) emitProgress(total int) {
	if d.opts.Progress == nil {
		return
	}
	d.opts.Progress(runevents.Progress{
		Stage:     runevents.StageUploading,
		Done:      d.report.RcloneResult.Transferred,
		Total:     int64(total),
		BytesDone: d.report.RcloneResult.Bytes,
	})
}

// putContent streams one content object's bytes to the receiver. The
// request is cancelled once the receiver has taken no bytes for
// c.stallTimeout — or, after the last byte, sent no reply for as long —
// so a peer that stops reading fails the upload instead of holding the
// run open forever.
func (c *nodeClient) putContent(ctx context.Context, receiverRunID int64, blake3Hex string, body io.Reader, size int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	guard := newStallGuard(ctx, c.stallTimeout, cancel)
	err := c.sendContent(ctx, syncproto.ContentPath(receiverRunID, blake3Hex), &progressReader{r: body, guard: guard}, size)
	cancel()
	guard.wait()
	if guard.fired.Load() {
		return fmt.Errorf("%w: %s took no bytes and sent no reply for %s", errUploadStalled, c.node.Name, c.stallTimeout)
	}
	return err
}

// sendContent issues the PUT. size is declared as Content-Length so the
// receiver bounds its read before the first byte arrives.
func (c *nodeClient) sendContent(ctx context.Context, urlPath string, body io.Reader, size int64) error {
	if size == 0 {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(urlPath), body)
	if err != nil {
		return fmt.Errorf("new request %s: %w", urlPath, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.node.Token)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("put %s: %w", urlPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode/100 == 2:
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusConflict:
		return &peerRejection{msg: urlPath + ": " + responseError(resp)}
	default:
		return fmt.Errorf("%s: %s", urlPath, responseError(resp))
	}
}

// progressReader pokes a stall guard each time the transport takes bytes
// from the upload body.
type progressReader struct {
	r     io.Reader
	guard *stallGuard
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.guard.advance()
	}
	return n, err
}

// maxPeerErrorBody bounds how much of a non-2xx reply is read before
// rendering it as a diagnostic. The receiver's error bodies are one JSON
// object with a single message; the cap keeps a misbehaving peer from
// streaming unbounded bytes into a run row.
const maxPeerErrorBody = 4 << 10

// responseError renders a non-2xx reply as a diagnostic, preferring the
// receiver's structured `error` field over a bare status line.
func responseError(resp *http.Response) string {
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxPeerErrorBody))
	var errBody syncproto.ErrorResponse
	_ = json.Unmarshal(bodyBytes, &errBody)
	if errBody.Error != "" {
		return fmt.Sprintf("%s (%d)", errBody.Error, resp.StatusCode)
	}
	return "status " + strconv.Itoa(resp.StatusCode)
}

// peerUploadStallTimeout is the no-progress bound on one content upload.
// It shares DefaultStallTimeout with scheduled rclone transfers and
// applies to foreground syncs too: a peer that takes no bytes for that
// long is wedged, and failing the upload lets the next run resume.
const peerUploadStallTimeout = DefaultStallTimeout
