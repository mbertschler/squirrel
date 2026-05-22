package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/mbertschler/squirrel/cmd/squirrel-desktop/app/templates"
	"github.com/mbertschler/squirrel/runevents"
	"github.com/mbertschler/squirrel/store"
)

// ServeEvents streams Server-Sent Events describing a run's progress.
// Each event is one Turbo Stream `replace` frame that swaps the
// run-status panel inside RunDetailPage. The connection closes when
// the run reaches a terminal status (the producer goroutine calls
// hub.Close, which closes the subscriber channel) or when the client
// disconnects (the request context is cancelled).
//
// Terminal runs short-circuit: we send the final frame derived from
// the stored row and return without registering a subscriber.
func (h *Runs) ServeEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	run, err := h.Store.GetRun(r.Context(), id)
	if err != nil {
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	setSSEHeaders(w)
	if isTerminal(run.Status) {
		_ = writeStatusFrame(r.Context(), w, h.runRow(r.Context(), run), runevents.Progress{Stage: runevents.StageDone})
		flusher.Flush()
		return
	}
	h.streamRunEvents(r.Context(), w, flusher, run, id)
}

// streamRunEvents subscribes to the progress hub for runID and pumps
// each event out as a turbo-stream frame. On every event we re-fetch
// the run row so the frame reflects the latest status (especially the
// terminal status that lands in the deferred FinishRun); on the
// channel close we emit one last frame and return.
func (h *Runs) streamRunEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, run store.Run, runID int64) {
	ch, cancel := h.hub.Subscribe(runID)
	defer cancel()
	row := h.runRow(ctx, run)
	if err := writeStatusFrame(ctx, w, row, runevents.Progress{Stage: runevents.StageWalking}); err != nil {
		return
	}
	flusher.Flush()
	for {
		select {
		case <-ctx.Done():
			return
		case p, ok := <-ch:
			if !ok {
				// Producer closed the feed: re-fetch so the final
				// status reflects the deferred FinishRun write, then
				// send one last frame and return.
				if final, err := h.Store.GetRun(ctx, runID); err == nil {
					row = h.runRow(ctx, final)
				}
				_ = writeStatusFrame(ctx, w, row, runevents.Progress{Stage: runevents.StageDone})
				flusher.Flush()
				return
			}
			// A progress event implies the run is still running; the
			// stored row's status hasn't transitioned yet so we don't
			// re-fetch on every tick (it would funnel one sqlite read
			// per second through the shared connection for nothing).
			if err := writeStatusFrame(ctx, w, row, p); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// setSSEHeaders applies the standard SSE response headers. Cache-Control
// is set to no-cache so a proxy can't satisfy the request from a buffer;
// Connection: keep-alive is informational under HTTP/1.1 but harmless
// under HTTP/2 where it's ignored.
func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// writeStatusFrame renders one turbo-stream frame as the body of one
// SSE event. The frame replaces the dom node with id="run-status"
// inside RunDetailPage. The output is prefixed with "data:" lines so
// browsers parse it as a single EventSource message; multi-line HTML
// is split into one data: line per source line.
func writeStatusFrame(ctx context.Context, w http.ResponseWriter, row templates.RunRow, p runevents.Progress) error {
	html, err := renderTemplToString(ctx, templates.RunStatusFrame(row, p))
	if err != nil {
		return err
	}
	return writeSSEEvent(w, html)
}

// isTerminal reports whether the run status is a final one — i.e. no
// further progress events will arrive. Mirrors runDone() in the
// templates package; duplicated to avoid the templates package
// depending on store directly.
func isTerminal(status string) bool {
	switch status {
	case store.RunStatusSuccess, store.RunStatusFailed, store.RunStatusPartial:
		return true
	}
	return false
}

// renderTemplToString renders a templ.Component into a string. Used to
// stage one turbo-stream frame for transport over SSE — the SSE wire
// format requires data: prefixing per line, which is easier to apply
// against a fully-rendered string than against a streamed writer.
func renderTemplToString(ctx context.Context, c templ.Component) (string, error) {
	var sb strings.Builder
	if err := c.Render(ctx, &sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// writeSSEEvent emits one SSE event whose body is body, escaping
// embedded newlines per the SSE wire format. The trailing blank line
// terminates the event.
func writeSSEEvent(w http.ResponseWriter, body string) error {
	// SSE wire format: each source line is one "data: ..." line; a
	// blank line terminates the event. Iterate manually rather than
	// regexp-splitting so an embedded "\r\n" doesn't create empty
	// lines that the browser would parse as event terminators.
	start := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '\n' {
			if _, err := fmt.Fprintf(w, "data: %s\n", body[start:i]); err != nil {
				return err
			}
			start = i + 1
		}
	}
	if start < len(body) {
		if _, err := fmt.Fprintf(w, "data: %s\n", body[start:]); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n")
	return err
}
