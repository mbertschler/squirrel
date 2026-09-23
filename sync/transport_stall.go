package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// errTransportStalled marks a transport call that made no progress for
// its allowance: a hung disk, or a server that stopped answering.
var errTransportStalled = errors.New("transport call made no progress")

// stalledCalls counts, per destination name, the transport calls a push
// gave up on that have not returned yet. A local call blocked in the
// kernel cannot be interrupted, so it may still complete a move after its
// push failed; a later push to the same destination refuses while any
// remain, so it never reconciles against a move still in flight.
var stalledCalls sync.Map // destination name → *atomic.Int64

func stalledCounter(destination string) *atomic.Int64 {
	c, _ := stalledCalls.LoadOrStore(destination, new(atomic.Int64))
	return c.(*atomic.Int64)
}

// stallBytesPerSecond is the slowest flush rate the allowance for a
// Put's final sync assumes, after its body has been read.
const stallBytesPerSecond = 1 << 20

// stallTransport bounds every call of the transport it wraps by progress
// rather than wall-clock: a call that makes none for timeout fails with
// errTransportStalled. A Put makes progress with every chunk it reads.
type stallTransport struct {
	transport
	timeout time.Duration
	stalled *atomic.Int64
}

// bounded runs call on its own goroutine and returns its result, or
// errTransportStalled once call went timeout without calling progress. A
// call given up on keeps its result to itself when it finally returns.
// progress resets the allowance to the duration it is given.
func bounded[T any](ctx context.Context, s stallTransport, what string, call func(ctx context.Context, progress func(time.Duration)) (T, error)) (T, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	allowance := make(chan time.Duration, 1)
	progress := func(d time.Duration) {
		select {
		case <-allowance:
		default:
		}
		allowance <- d
	}
	type result struct {
		v   T
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, err := call(ctx, progress)
		done <- result{v, err}
	}()
	timer := time.NewTimer(s.timeout)
	defer timer.Stop()
	for {
		select {
		case r := <-done:
			return r.v, r.err
		case d := <-allowance:
			timer.Reset(d)
		case <-timer.C:
			s.stalled.Add(1)
			go func() { <-done; s.stalled.Add(-1) }()
			var zero T
			return zero, fmt.Errorf("%w: %s, none for %s", errTransportStalled, what, s.timeout)
		}
	}
}

func (s stallTransport) Stat(ctx context.Context, name string) (entry, error) {
	return bounded(ctx, s, "stat "+name, func(ctx context.Context, _ func(time.Duration)) (entry, error) {
		return s.transport.Stat(ctx, name)
	})
}

func (s stallTransport) List(ctx context.Context, dir string) ([]entry, error) {
	return bounded(ctx, s, "list "+dir, func(ctx context.Context, _ func(time.Duration)) ([]entry, error) {
		return s.transport.List(ctx, dir)
	})
}

// Get bounds the open, and every Read of the file it returns, by the same
// allowance.
func (s stallTransport) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	rc, err := bounded(ctx, s, "get "+name, func(ctx context.Context, _ func(time.Duration)) (io.ReadCloser, error) {
		return s.transport.Get(ctx, name)
	})
	if err != nil {
		return nil, err
	}
	return &stallReader{ctx: ctx, s: s, name: name, rc: rc}, nil
}

func (s stallTransport) Put(ctx context.Context, name string, r io.Reader, mtime time.Time) error {
	_, err := bounded(ctx, s, "put "+name, func(ctx context.Context, progress func(time.Duration)) (struct{}, error) {
		return struct{}{}, s.transport.Put(ctx, name, &progressingReader{r: r, timeout: s.timeout, progress: progress}, mtime)
	})
	return err
}

func (s stallTransport) Rename(ctx context.Context, from, to string) error {
	_, err := bounded(ctx, s, "rename "+from, func(ctx context.Context, _ func(time.Duration)) (struct{}, error) {
		return struct{}{}, s.transport.Rename(ctx, from, to)
	})
	return err
}

func (s stallTransport) Remove(ctx context.Context, name string) error {
	_, err := bounded(ctx, s, "remove "+name, func(ctx context.Context, _ func(time.Duration)) (struct{}, error) {
		return struct{}{}, s.transport.Remove(ctx, name)
	})
	return err
}

// stallReader bounds each Read of a file Get opened. A Read runs into the
// reader's own buffer, so one given up on can finish later without
// touching the caller's; after that the reader only reports the stall.
type stallReader struct {
	ctx  context.Context
	s    stallTransport
	name string
	rc   io.ReadCloser
	buf  []byte
	err  error // the stall, once one happened
}

func (r *stallReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if cap(r.buf) < len(p) {
		r.buf = make([]byte, len(p))
	}
	buf := r.buf[:len(p)]
	n, err := bounded(r.ctx, r.s, "read "+r.name, func(context.Context, func(time.Duration)) (int, error) {
		return r.rc.Read(buf)
	})
	if errors.Is(err, errTransportStalled) {
		r.err = err
		return 0, err
	}
	return copy(p, buf[:n]), err
}

func (r *stallReader) Close() error {
	if r.err != nil {
		go func() { _ = r.rc.Close() }()
		return nil
	}
	return r.rc.Close()
}

// progressingReader reports each chunk it reads as progress, and at the
// end of the body allows the final sync time to flush what it read.
type progressingReader struct {
	r        io.Reader
	timeout  time.Duration
	progress func(time.Duration)
	n        int64
}

func (p *progressingReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	switch {
	case errors.Is(err, io.EOF):
		p.progress(p.timeout + time.Duration(p.n/stallBytesPerSecond)*time.Second)
	case n > 0:
		p.progress(p.timeout)
	}
	return n, err
}
