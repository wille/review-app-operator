package utils

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// serve starts Serve with handler and returns the URL it serves on and a channel that
// receives what Serve returns
func serve(t *testing.T, ctx context.Context, handler http.HandlerFunc, serving *Serving, timeout time.Duration) (string, <-chan error) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, ln, &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}, serving, timeout)
	}()

	return "http://" + ln.Addr().String(), done
}

func TestServeIsOnlyReadyWhileServing(t *testing.T) {
	serving := &Serving{}
	if serving.Check(nil) == nil {
		t.Fatal("expected the server to be unready before it serves")
	}

	ctx, cancel := context.WithCancel(context.Background())
	url, done := serve(t, ctx, func(w http.ResponseWriter, r *http.Request) {}, serving, time.Second)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if err := serving.Check(nil); err != nil {
		t.Fatalf("expected the server to be ready while it serves, got %v", err)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if serving.Check(nil) == nil {
		t.Fatal("expected the server to be unready after it shut down")
	}
}

func TestServeFinishesRequestsInFlight(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})

	serving := &Serving{}
	ctx, cancel := context.WithCancel(context.Background())
	url, done := serve(t, ctx, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte("deployed"))
	}, serving, 10*time.Second)

	responses := make(chan string, 1)
	go func() {
		resp, err := http.Get(url)
		if err != nil {
			responses <- err.Error()
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		responses <- string(body)
	}()

	<-started
	cancel()

	// The server stops taking traffic, but waits for the request in flight
	time.Sleep(100 * time.Millisecond)
	if serving.Check(nil) == nil {
		t.Fatal("expected the server to be unready once it shuts down")
	}
	select {
	case err := <-done:
		t.Fatalf("expected Serve to wait for the request in flight, it returned %v", err)
	default:
	}

	close(release)

	if body := <-responses; body != "deployed" {
		t.Fatalf("expected the request in flight to finish, got %q", body)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeCutsOffRequestsAfterTheShutdownTimeout(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	url, done := serve(t, ctx, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
	}, &Serving{}, 100*time.Millisecond)

	go func() {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected Serve to return after the shutdown timeout")
	}
}
