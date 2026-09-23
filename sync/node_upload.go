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
	"sync"
	"time"

	"github.com/zeebo/blake3"

	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/syncproto"
)

// peerUploadStallTimeout is the no-progress bound on one content upload.
// It shares DefaultStallTimeout with scheduled rclone transfers and
// applies to foreground syncs too: a peer that takes no bytes for that
// long is wedged, and failing the upload lets the next run resume.
const peerUploadStallTimeout = DefaultStallTimeout

// minFanOutBytesPerSecond is the slowest local copy rate the reply
// allowance assumes for a receiver fanning one upload out to duplicate
// paths before it answers.
const minFanOutBytesPerSecond = 10 << 20

// maxConsecutiveUploadFailures is how many uploads in a row may fail on
// the receiver's side or in transit before the transfer phase gives up on
// the peer.
const maxConsecutiveUploadFailures = 3

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
// for, or stopped answering, for longer than its allowance.
var errUploadStalled = errors.New("upload stalled")

// peerStatusError is a non-2xx reply to an upload.
type peerStatusError struct {
	status int
	msg    string
}

func (e *peerStatusError) Error() string { return e.msg }

// uploadFailure is why one path's upload failed, and whether sending it
// again later in the same run could change the outcome.
type uploadFailure struct {
	msg       string
	retryable bool
}

// uploadJob is one content object to send: the paths awaiting it, each a
// candidate source in order.
type uploadJob struct {
	rels []string
	plannedContent
}

// uploadPaths delivers the content behind the given volume-relative
// paths. Paths sharing a BLAKE3 collapse to one upload — the receiver
// materialises every path in the session that wants those bytes — so a
// volume with duplicate files transfers each distinct content once.
//
// A failed upload costs only its own paths: it is recorded, /verify
// reports the paths, and one that still fails leaves the run partial with
// everything else committed. The phase ends early only when the peer
// itself is unwell — a stalled upload, a session the receiver no longer
// holds, or maxConsecutiveUploadFailures receiver or transport failures in
// a row — and the receiver keeps what already landed.
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
	consecutive := 0
	for _, job := range jobs {
		err := d.uploadOne(job)
		switch {
		case err == nil:
			consecutive = 0
			d.noteUploaded(job, len(jobs))
		case confinedToContent(err):
			d.noteUploadFailure(job, err)
		case d.endsTransfer(err) || consecutive+1 >= maxConsecutiveUploadFailures:
			return fmt.Errorf("upload %s: %w", job.rels[0], err)
		default:
			consecutive++
			d.noteUploadFailure(job, err)
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
	f, rel, err := d.openUploadSource(job)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	src := &uploadSource{f: f, rel: rel, remaining: job.size}
	hasher := blake3.New()
	putErr := d.client.putContent(d.ctx, d.receiverRunID, job.hashHex, io.TeeReader(src, hasher), job.size, d.replyAllowance(job))
	if src.remaining == 0 && (putErr == nil || isRejection(putErr)) {
		if sent := hex.EncodeToString(hasher.Sum(nil)); sent != job.hashHex {
			return fmt.Errorf("%w: %s hashed to %s while sending, indexed as %s — run `squirrel index %s` and sync again",
				errContentDrift, rel, sent, job.hashHex, d.vol.Name)
		}
	}
	return putErr
}

// openUploadSource opens the first of job's paths still holding a regular
// file at least the planned size, so a duplicate deleted since indexing
// does not stop the others' content from moving.
func (d *nodeSyncDriver) openUploadSource(job uploadJob) (*os.File, string, error) {
	var firstErr error
	for _, rel := range job.rels {
		f, err := openRegularAtLeast(filepath.Join(d.vol.Path, rel), job.size)
		if err == nil {
			return f, rel, nil
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", rel, err)
		}
	}
	return nil, "", firstErr
}

func openRegularAtLeast(abs string, size int64) (*os.File, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errUploadSource, err)
	}
	info, err := f.Stat()
	switch {
	case err != nil:
		err = fmt.Errorf("%w: %w", errUploadSource, err)
	case !info.Mode().IsRegular():
		err = fmt.Errorf("%w: no longer a regular file", errUploadSource)
	case info.Size() < size:
		err = fmt.Errorf("%w: %d bytes, indexed as %d — re-index and sync again", errContentDrift, info.Size(), size)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// uploadSource reads exactly remaining bytes of a source file, tagging a
// read failure as errUploadSource and a file that ends early as
// errContentDrift, so either costs only its own paths.
type uploadSource struct {
	f         *os.File
	rel       string
	remaining int64
}

func (s *uploadSource) Read(b []byte) (int, error) {
	if s.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > s.remaining {
		b = b[:s.remaining]
	}
	n, err := s.f.Read(b)
	s.remaining -= int64(n)
	switch {
	case errors.Is(err, io.EOF) && s.remaining > 0:
		return n, fmt.Errorf("%w: %s ended %d bytes short of its indexed size", errContentDrift, s.rel, s.remaining)
	case err != nil && !errors.Is(err, io.EOF):
		return n, fmt.Errorf("%w: %s: %w", errUploadSource, s.rel, err)
	}
	return n, err
}

// replyAllowance is how long the receiver may take to answer once it has
// every byte: the stall bound, plus time to copy the object to each
// duplicate path it fans out to.
func (d *nodeSyncDriver) replyAllowance(job uploadJob) time.Duration {
	copyBytes := job.size * int64(len(job.rels)-1)
	return d.client.stallTimeout + time.Duration(copyBytes/minFanOutBytesPerSecond)*time.Second
}

func isRejection(err error) bool {
	var status *peerStatusError
	return errors.As(err, &status) && (status.status == http.StatusBadRequest || status.status == http.StatusConflict)
}

func confinedToContent(err error) bool {
	return errors.Is(err, errUploadSource) || errors.Is(err, errContentDrift) || isRejection(err)
}

// endsTransfer reports whether err means no later upload in this session
// can succeed either.
func (d *nodeSyncDriver) endsTransfer(err error) bool {
	if d.ctx.Err() != nil || errors.Is(err, errUploadStalled) {
		return true
	}
	var status *peerStatusError
	if !errors.As(err, &status) {
		return false
	}
	switch status.status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

func (d *nodeSyncDriver) noteUploaded(job uploadJob, total int) {
	d.report.RcloneResult.Transferred++
	d.report.RcloneResult.Bytes += job.size
	for _, rel := range job.rels {
		delete(d.uploadFailures, rel)
	}
	d.emitProgress(total)
}

func (d *nodeSyncDriver) noteUploadFailure(job uploadJob, err error) {
	if d.uploadFailures == nil {
		d.uploadFailures = make(map[string]uploadFailure)
	}
	retryable := !errors.Is(err, errUploadSource) && !errors.Is(err, errContentDrift)
	for _, rel := range job.rels {
		d.uploadFailures[rel] = uploadFailure{msg: err.Error(), retryable: retryable}
	}
}

// retryable narrows failing to the paths worth sending again this run: a
// source that vanished or changed stays that way until the next index.
func (d *nodeSyncDriver) retryable(failing []string) []string {
	out := make([]string, 0, len(failing))
	for _, rel := range failing {
		if failure, ok := d.uploadFailures[rel]; ok && !failure.retryable {
			continue
		}
		out = append(out, rel)
	}
	return out
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
		msg := "content did not verify on " + d.node.Name
		if failure, ok := d.uploadFailures[rel]; ok {
			msg = failure.msg
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
// c.stallTimeout, or has not answered within replyAllowance of the last
// one, so a peer that stops reading fails the upload instead of holding
// the run open forever.
func (c *nodeClient) putContent(ctx context.Context, receiverRunID int64, blake3Hex string, body io.Reader, size int64, replyAllowance time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := newUploadWatchdog(cancel, c.stallTimeout, replyAllowance)
	err := c.sendContent(ctx, syncproto.ContentPath(receiverRunID, blake3Hex), &progressReader{r: body, watchdog: watchdog}, size)
	if watchdog.stop() {
		return fmt.Errorf("%w: %s stopped taking or answering the upload of %s", errUploadStalled, c.node.Name, blake3Hex)
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
	if resp.StatusCode/100 != 2 {
		return &peerStatusError{status: resp.StatusCode, msg: urlPath + ": " + responseError(resp)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// uploadWatchdog cancels an upload that goes idle for longer than its
// allowance: idle while the body is being sent, reply once all of it has.
type uploadWatchdog struct {
	idle  time.Duration
	reply time.Duration
	timer *time.Timer
	mu    sync.Mutex
	fired bool
}

func newUploadWatchdog(cancel context.CancelFunc, idle, reply time.Duration) *uploadWatchdog {
	w := &uploadWatchdog{idle: idle, reply: reply}
	w.timer = time.AfterFunc(idle, func() {
		w.mu.Lock()
		w.fired = true
		w.mu.Unlock()
		cancel()
	})
	return w
}

func (w *uploadWatchdog) progress() { w.timer.Reset(w.idle) }

func (w *uploadWatchdog) bodySent() { w.timer.Reset(w.reply) }

// stop disarms the watchdog and reports whether it had fired.
func (w *uploadWatchdog) stop() bool {
	w.timer.Stop()
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

type progressReader struct {
	r        io.Reader
	watchdog *uploadWatchdog
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.watchdog.progress()
	}
	if errors.Is(err, io.EOF) {
		p.watchdog.bodySent()
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
