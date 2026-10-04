package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"regexp"
)

// requestIDHeader carries a request's ID both ways: a client may name its
// request, and every response names the ID the gateway knows it by.
const requestIDHeader = "X-Request-Id"

// clientRequestIDRE matches a client's ID the gateway adopts as it is. The
// ID reaches response headers and bodies, the request log, usage records and
// audit history, so it is short and holds nothing that could break a header
// or a log line.
var clientRequestIDRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type requestIDKey struct{}

// assignRequestIDs gives every request an ID: the client's when it matches
// clientRequestIDRE, a new one otherwise. The ID goes on the response before
// any handler runs, so an error or a stream carries it as well, and into the
// request's context, where whatever the request records reads it.
func assignRequestIDs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !clientRequestIDRE.MatchString(id) {
			id = newRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// requestIDFrom returns the ID of the request ctx belongs to, or "" for work
// no request started.
func requestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// newRequestID returns a fresh ID. Its 128 random bits make it practically
// certain that no other ID the gateway assigns is the same.
func newRequestID() string {
	var random [16]byte
	_, _ = rand.Read(random[:])
	return "req_" + hex.EncodeToString(random[:])
}
