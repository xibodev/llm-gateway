package main

import (
	"net/http"
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
