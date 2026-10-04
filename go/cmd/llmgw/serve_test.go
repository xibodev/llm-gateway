package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHTTPServerClosesIdleConnectionsWithoutCuttingStreams(t *testing.T) {
	srv := newHTTPServer(http.NotFoundHandler())
	if srv.ReadHeaderTimeout != 10*time.Second || srv.IdleTimeout != 3*time.Minute {
		t.Fatalf("header timeout %s, idle timeout %s", srv.ReadHeaderTimeout, srv.IdleTimeout)
	}
	// Either would end a streamed response that outlives it.
	if srv.ReadTimeout != 0 || srv.WriteTimeout != 0 {
		t.Fatalf("read timeout %s, write timeout %s", srv.ReadTimeout, srv.WriteTimeout)
	}
}

func TestShutdownTimeoutFollowsTheEnvironment(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", defaultShutdownTimeout},
		{"40", 40 * time.Second},
		{" 5 ", 5 * time.Second},
		{"0", 0},
		{"-1", defaultShutdownTimeout},
		{"2.5", defaultShutdownTimeout},
		{"soon", defaultShutdownTimeout},
	} {
		t.Setenv("LLMGW_SHUTDOWN_TIMEOUT_SECONDS", test.value)
		if got := shutdownTimeout(); got != test.want {
			t.Errorf("LLMGW_SHUTDOWN_TIMEOUT_SECONDS=%q gives %s, want %s", test.value, got, test.want)
		}
	}
	if defaultShutdownTimeout != 25*time.Second {
		t.Fatalf("default drain %s", defaultShutdownTimeout)
	}
}

// startServing serves handler on a free loopback port through
// serveUntilShutdown, which returns on the channel once shutdown is called.
func startServing(t *testing.T, handler http.Handler, drain time.Duration) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, shutdown := context.WithCancel(context.Background())
	t.Cleanup(shutdown)
	result := make(chan error, 1)
	go func() { result <- serveUntilShutdown(ctx, newHTTPServer(handler), listener, drain) }()
	return listener.Addr().String(), shutdown, result
}

func testClient(t *testing.T) *http.Client {
	transport := &http.Transport{}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport}
}

func waitFor(t *testing.T, done <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}

// A stop closes the listener at once but lets a stream already in flight run
// to its end inside the drain.
func TestShutdownDrainsStreamsInFlightAndRefusesNewConnections(t *testing.T) {
	streaming, release := make(chan struct{}), make(chan struct{})
	address, shutdown, result := startServing(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()
		close(streaming)
		<-release
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}), 10*time.Second)
	response, err := testClient(t).Get("http://" + address + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	waitFor(t, streaming, "the stream did not start")
	shutdown()

	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("new connections are still accepted during shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-result:
		t.Fatalf("shutdown returned with a stream still open: %v", err)
	default:
	}

	close(release)
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "data: first\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream body=%q err=%v", body, err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("a drained shutdown returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not return once the stream finished")
	}
}

// A request that outlasts the drain is closed, and its handler sees the
// cancellation, rather than holding the process up.
func TestShutdownClosesRequestsThatOutlastTheDrain(t *testing.T) {
	streaming, cancelled := make(chan struct{}), make(chan struct{})
	address, shutdown, result := startServing(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data: first\n\n")
		_ = http.NewResponseController(w).Flush()
		close(streaming)
		<-r.Context().Done()
		close(cancelled)
	}), 50*time.Millisecond)
	response, err := testClient(t).Get("http://" + address + "/v1/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	waitFor(t, streaming, "the stream did not start")
	shutdown()

	select {
	case err := <-result:
		if !errors.Is(err, errDrainExpired) {
			t.Fatalf("shutdown returned %v, want an expired drain", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown waited past its drain")
	}
	waitFor(t, cancelled, "the request cut off by the drain was not cancelled")
	if _, err := io.ReadAll(response.Body); err == nil {
		t.Fatal("a stream cut off by the drain ended cleanly")
	}
}

// A failure to serve comes back to the caller, which still has workers to
// stop and state to release, instead of ending the process.
func TestServeFailureIsReturned(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = listener.Close()
	err = serveUntilShutdown(context.Background(), newHTTPServer(http.NotFoundHandler()), listener, time.Second)
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("serving on a closed listener returned %v", err)
	}
}

func TestSecondSignalForcesExit(t *testing.T) {
	relay := func(signals <-chan os.Signal) (shutdown, forced, returned chan struct{}) {
		shutdown, forced, returned = make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			relayShutdownSignals(signals, func() { close(shutdown) }, func() { close(forced) })
			close(returned)
		}()
		return shutdown, forced, returned
	}

	signals := make(chan os.Signal, 1)
	shutdown, forced, returned := relay(signals)
	signals <- os.Interrupt
	waitFor(t, shutdown, "the first signal did not start the shutdown")
	select {
	case <-forced:
		t.Fatal("the first signal forced an exit")
	default:
	}
	signals <- syscall.SIGTERM
	waitFor(t, forced, "the second signal did not force an exit")
	waitFor(t, returned, "the relay did not return")

	// Once the gateway stops listening, the relay ends without forcing.
	signals = make(chan os.Signal, 1)
	shutdown, forced, returned = relay(signals)
	signals <- syscall.SIGTERM
	waitFor(t, shutdown, "the first signal did not start the shutdown")
	close(signals)
	waitFor(t, returned, "the relay did not return once stopped")
	select {
	case <-forced:
		t.Fatal("stopping the relay forced an exit")
	default:
	}
}

// The port is bound before any background worker starts, so a busy port
// fails the start first: the error names the port even though the external
// key file, which the first worker reads, is missing too.
func TestServeFailsOnABusyPortBeforeStartingWorkers(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, _ := net.SplitHostPort(busy.Addr().String())
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", filepath.Join(state, "config.yaml"))
	t.Setenv("LLMGW_CONFIG_SEED", "")
	t.Setenv("LLMGW_HOST", "127.0.0.1")
	t.Setenv("LLMGW_PORT", port)
	t.Setenv("LLMGW_PROVIDER_ROSTER_DISABLE", "1")
	t.Setenv("LLMGW_EXTERNAL_KEYS_FILE", filepath.Join(state, "missing-keys.json"))
	t.Setenv("LLMGW_EXTERNAL_KEYS_URL", "")

	err = serve()
	if err == nil || !strings.Contains(err.Error(), "listen on 127.0.0.1:"+port) {
		t.Fatalf("serve on a busy port returned %v", err)
	}
}
