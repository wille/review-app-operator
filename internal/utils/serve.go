package utils

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Serving reports whether a server accepts requests, for its readiness probe
type Serving struct {
	serving atomic.Bool
}

func (s *Serving) set(serving bool) {
	if s != nil {
		s.serving.Store(serving)
	}
}

// Check fails until the server listens, and again once it shuts down, so that it only
// gets traffic while it accepts requests
func (s *Serving) Check(_ *http.Request) error {
	if s == nil || !s.serving.Load() {
		return errors.New("not serving requests")
	}
	return nil
}

// Serve serves requests on the listener until ctx is done. It then stops accepting requests and
// waits up to shutdownTimeout for the requests in flight, like deploys that stream their progress,
// before returning.
func Serve(ctx context.Context, ln net.Listener, srv *http.Server, serving *Serving, shutdownTimeout time.Duration) error {
	serving.set(true)

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		serving.set(false)

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// The requests still in flight are cut off
			_ = srv.Close()
		}
	}()

	// Serve returns as soon as the shutdown starts, before the requests in flight have finished
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		serving.set(false)
		return err
	}

	<-stopped
	return nil
}
