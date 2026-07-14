package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/mbertschler/squirrel/runevents"
)

// stdoutIsTTY reports whether the process's stdout is attached to a real
// terminal. Progress is auto-enabled on a TTY and stays quiet otherwise
// (pipes, files, cron, the agent path) so logs and the runs table keep
// getting only the final summary line.
func stdoutIsTTY() bool {
	return isatty.IsTerminal(os.Stdout.Fd())
}

// progressEnabled decides whether a command should render live progress.
// An explicit --progress / -P (or --progress=false) always wins; with the
// flag untouched it auto-enables when stdout is a TTY. This is the single
// gate both `index` and `sync` consult, and it is what keeps scripted and
// agent runs quiet without any per-caller special-casing.
func progressEnabled(cmd *cobra.Command, flag bool) bool {
	if cmd.Flags().Changed("progress") {
		return flag
	}
	return stdoutIsTTY()
}

// progressPrinter renders throttled, single-line progress to w, overwriting
// the same terminal line with a carriage return on each update and erasing
// it on clear() so the final summary prints on a clean line. It is fed by
// the runevents.Progress callback the index/sync libraries invoke; all
// human formatting (byte units, rate, ETA) lives here, never in a library
// package. The library side already throttles emissions, so update() just
// renders whatever it is handed.
type progressPrinter struct {
	w     io.Writer
	start time.Time

	mu      sync.Mutex
	lastLen int
	active  bool
}

func newProgressPrinter(w io.Writer) *progressPrinter {
	return &progressPrinter{w: w, start: time.Now()}
}

// update renders one progress datapoint. It is safe to call from the
// library's progress goroutine; clear() from the main goroutine is
// serialised against it via mu.
func (p *progressPrinter) update(ev runevents.Progress) {
	line := p.format(ev)
	p.mu.Lock()
	defer p.mu.Unlock()
	width := utf8.RuneCountInString(line)
	pad := ""
	if d := p.lastLen - width; d > 0 {
		pad = strings.Repeat(" ", d)
	}
	fmt.Fprintf(p.w, "\r%s%s", line, pad)
	p.lastLen = width
	p.active = true
}

// clear erases the current progress line so the following output starts on
// a fresh line. A no-op if nothing has been rendered yet.
func (p *progressPrinter) clear() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.active {
		return
	}
	fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", p.lastLen))
	p.active = false
	p.lastLen = 0
}

// format renders one event into a single line. Stage selects the shape:
// the index path reports counts + throughput + current path (no total is
// known up front), while the sync path reports done/total files and bytes
// with an ETA once rclone has announced the total.
func (p *progressPrinter) format(ev runevents.Progress) string {
	rate := p.rate(ev.BytesDone)
	switch ev.Stage {
	case runevents.StageUploading:
		return p.formatUploading(ev, rate)
	default:
		return p.formatHashing(ev, rate)
	}
}

func (p *progressPrinter) formatHashing(ev runevents.Progress, rate float64) string {
	line := fmt.Sprintf("indexing: %d files · %s · %s",
		ev.Done, humanBytes(ev.BytesDone), humanRate(rate))
	if ev.Message != "" {
		line += " · " + truncatePath(ev.Message, 48)
	}
	return line
}

func (p *progressPrinter) formatUploading(ev runevents.Progress, rate float64) string {
	files := fmt.Sprintf("%d", ev.Done)
	if ev.Total > 0 {
		files = fmt.Sprintf("%d/%d", ev.Done, ev.Total)
	}
	bytesPart := humanBytes(ev.BytesDone)
	if ev.BytesTotal > 0 {
		bytesPart = fmt.Sprintf("%s/%s", humanBytes(ev.BytesDone), humanBytes(ev.BytesTotal))
	}
	line := fmt.Sprintf("syncing: %s files · %s · %s", files, bytesPart, humanRate(rate))
	if eta, ok := p.eta(ev, rate); ok {
		line += " · ETA " + eta
	}
	return line
}

// rate is the mean throughput since the printer started, in bytes/sec.
// Wall-clock mean (rather than an instantaneous delta) is stable enough for
// a once-per-second line and needs no inter-call state.
func (p *progressPrinter) rate(bytesDone int64) float64 {
	elapsed := time.Since(p.start).Seconds()
	if elapsed <= 0 {
		return 0
	}
	return float64(bytesDone) / elapsed
}

// eta estimates remaining time from the mean rate and the announced byte
// total. Returns ok=false when no total is known or the rate is not yet
// meaningful, so the caller omits the ETA rather than printing a bogus one.
func (p *progressPrinter) eta(ev runevents.Progress, rate float64) (string, bool) {
	if ev.BytesTotal <= 0 || rate <= 0 || ev.BytesDone >= ev.BytesTotal {
		return "", false
	}
	remaining := float64(ev.BytesTotal-ev.BytesDone) / rate
	return humanDuration(time.Duration(remaining) * time.Second), true
}

// humanBytes renders a byte count with binary (IEC) units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// humanRate renders a throughput in bytes/sec, or an em dash before any
// throughput is known.
func humanRate(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "— /s"
	}
	return humanBytes(int64(bytesPerSec)) + "/s"
}

// humanDuration renders a duration at second granularity (e.g. "2m30s").
func humanDuration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	return d.Truncate(time.Second).String()
}

// truncatePath shortens a path to at most max runes, keeping the tail (the
// filename end is the useful part) behind a leading ellipsis.
func truncatePath(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[len(r)-max:])
	}
	return "…" + string(r[len(r)-(max-1):])
}
