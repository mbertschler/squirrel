package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// shutdownGrace is how long Serve will wait for in-flight requests to
// drain after the context is cancelled before forcing the listener
// closed. Five seconds is enough for a single in-flight `rclone copy`
// signaller round-trip in PR 3; longer-running streaming endpoints will
// need to revisit.
const shutdownGrace = 5 * time.Second

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
// :0, observe the resolved port via ln.Addr(), and drive the daemon
// end-to-end without binding a fixed port.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpSrv := &http.Server{Handler: s.handler}
	errCh := make(chan error, 1)
	go func() {
		errCh <- runServer(httpSrv, ln, s.cfg.TLSCert, s.cfg.TLSKey)
	}()

	select {
	case <-ctx.Done():
		return gracefulShutdown(httpSrv, errCh)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
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
