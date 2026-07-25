package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// shutdownGrace is how long Serve will wait for in-flight requests to
// drain after the context is cancelled before forcing the listener
// closed. Five seconds is enough for a single in-flight `rclone copy`
// signaller round-trip in PR 3; longer-running streaming endpoints will
// need to revisit.
const shutdownGrace = 5 * time.Second

// readHeaderTimeout caps how long a client can take to send request
// headers, the principal slowloris mitigation for net/http. Reasonable
// floor for the LAN/VPN exposure model this agent is built for.
//
// We deliberately don't set ReadTimeout or WriteTimeout: plan
// negotiation (PR 3) streams the initiator's index slice and the
// receiver's reconciliation report over the same connection, and either
// can outlast a hard wall-clock cap on realistic volumes. Header timing
// alone is enough to keep half-open connections from accumulating.
//
// idleTimeout closes connections kept alive between requests after a
// reasonable quiet period so we don't leak fds to clients that go away
// without an explicit close.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// ListenAndServe opens a TCP listener on Config.Listen and serves it.
// Returns when ctx is cancelled (after a graceful shutdown) or when the
// underlying http.Server fails to start.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.cfg.Listen, err)
	}
	return s.Serve(ctx, ln)
}

// Serve runs the HTTP server on the supplied listener. The caller owns ln
// only up to this call; Serve closes it on shutdown (via http.Server.Shutdown).
//
// The dispatch between plain HTTP and TLS is decided by whether the
// Config has a cert/key pair. Tests can construct a listener bound to
// :0, observe the resolved port via ln.Addr(), and drive the agent
// end-to-end without binding a fixed port.
//
// When Config.ScanInterval is non-zero the drift-detection scheduler
// (#17) runs in a sibling goroutine off the same context; when any
// volume declares a sync_every / index_every cadence the cadence
// scheduler (#39) runs in another sibling. Cancelling ctx stops all of
// them.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(httpSrv, ln, s.cfg.TLSCert, s.cfg.TLSKey)
	}()

	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	var loopWG sync.WaitGroup
	s.startScanLoop(loopCtx, &loopWG, s.scanLogger())
	s.startSchedulerLoop(loopCtx, &loopWG)

	select {
	case <-ctx.Done():
		err := gracefulShutdown(httpSrv, errCh)
		cancelLoops()
		loopWG.Wait()
		return err
	case err := <-errCh:
		cancelLoops()
		loopWG.Wait()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// RunSchedulers runs the background scan + cadence loops without binding
// an HTTP listener — the listener-less agent mode (F35) for cadence-only
// machines that never receive peer syncs. It blocks until ctx is
// cancelled, then waits for an in-flight loop tick to finish its volume,
// mirroring Serve's shutdown discipline minus the HTTP server. The two
// loops are gated exactly as under Serve (scan only when ScanInterval is
// set; scheduler only when a volume declares a cadence), so an agent with
// nothing scheduled runs no goroutines and simply waits for cancellation.
func (s *Server) RunSchedulers(ctx context.Context) error {
	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()
	var loopWG sync.WaitGroup
	s.startScanLoop(loopCtx, &loopWG, s.scanLogger())
	s.startSchedulerLoop(loopCtx, &loopWG)
	<-ctx.Done()
	cancelLoops()
	loopWG.Wait()
	return nil
}

// startScanLoop spins up the drift-detection scheduler in a sibling
// goroutine, but only when ScanInterval is set. The WaitGroup lets
// Serve block on the loop's clean exit during shutdown so a tick
// already-in-flight finishes its volume before the function returns.
func (s *Server) startScanLoop(ctx context.Context, wg *sync.WaitGroup, logger io.Writer) {
	if s.cfg.ScanInterval <= 0 {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runScanLoop(ctx, logger)
	}()
}

// startSchedulerLoop spins up the cadence scheduler (#39) in a sibling
// goroutine, but only when there is scheduled work: a volume sync_every /
// index_every cadence, a destination verify cadence (F32), or a peer
// durability-pull cadence (F33). The WaitGroup parallel to startScanLoop's
// usage lets Serve block on a clean exit during shutdown.
func (s *Server) startSchedulerLoop(ctx context.Context, wg *sync.WaitGroup) {
	sched := newScheduler(s, s.cfg.SchedulerTick, s.cfg.Now)
	if !sched.anyScheduledWork() {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		sched.run(ctx)
	}()
}

// scanLogger picks the destination for the scan loop's one-line
// updates. The Config field wins; nil falls back to io.Discard so
// the loop never panics for callers that left it unset.
func (s *Server) scanLogger() io.Writer {
	if s.cfg.ScanLogger != nil {
		return s.cfg.ScanLogger
	}
	return io.Discard
}

// runServer dispatches to ServeTLS when a cert/key pair is set, plain
// Serve otherwise. Pulled out of Serve so the select{} above stays
// compact and the two code paths are obvious in one place.
func runServer(srv *http.Server, ln net.Listener, certPath, keyPath string) error {
	if certPath != "" {
		return srv.ServeTLS(ln, certPath, keyPath)
	}
	return srv.Serve(ln)
}

// gracefulShutdown initiates Shutdown with a bounded grace window and
// waits for the Serve goroutine to return. Shutdown sets ErrServerClosed
// on the Serve result, which we filter out as a clean exit.
func gracefulShutdown(srv *http.Server, errCh <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
