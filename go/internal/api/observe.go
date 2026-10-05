package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

// requestObservation is what one request learns while it is served: who
// made it and what served it. It is read once the request ends, for the
// access log line and the request's metrics.
type requestObservation struct {
	mu sync.Mutex
	observed
}

// observed are the fields of a requestObservation.
type observed struct {
	caller         string
	projectID      string
	keyID          string
	keyFingerprint string
	provider       string
	model          string
	upstreamStatus int
	inputTokens    int64
	outputTokens   int64
}

type observationKey struct{}

// observations finds a request's observation by its request ID for the
// usage records the handlers write, which carry the ID but not the request.
var observations sync.Map // request ID -> *requestObservation

func observationFrom(ctx context.Context) *requestObservation {
	observation, _ := ctx.Value(observationKey{}).(*requestObservation)
	return observation
}

// observeCaller notes who r was authenticated as. A static administrator
// key is named by its fingerprint, as the audit log names it, never by its
// value.
func observeCaller(r *http.Request, p *config.Principal) {
	observation := observationFrom(r.Context())
	if observation == nil || p == nil {
		return
	}
	caller := callerKind(p)
	fingerprint := ""
	if caller == "admin_key" {
		fingerprint = staticKeyFingerprint(extractAPIKey(r))
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	observation.caller, observation.projectID, observation.keyID, observation.keyFingerprint = caller, p.ProjectID, p.KeyID, fingerprint
}

// callerKind names the kind of credential a request was served under.
func callerKind(p *config.Principal) string {
	switch {
	case p.Caller.ID == iam.AdminCallerID:
		return "admin_key"
	case p.Caller.Kind == core.LocalCaller().Kind:
		return "local"
	case p.KeyID != "":
		return "project_key"
	case p.ProjectID != "":
		return "external_key"
	}
	return "other"
}

// recordUsage records a usage event and notes, on the request it belongs
// to, the target that served it and the tokens it used.
func recordUsage(record router.UsageRecord) {
	router.RecordUsage(record)
	if record.RequestID == "" {
		return
	}
	value, ok := observations.Load(record.RequestID)
	if !ok {
		return
	}
	observation := value.(*requestObservation)
	observation.mu.Lock()
	defer observation.mu.Unlock()
	if record.Provider != "" {
		observation.provider, observation.model = record.Provider, record.RoutedModel
	}
	observation.upstreamStatus = record.StatusCode
	observation.inputTokens += int64(record.InputTokens)
	observation.outputTokens += int64(record.OutputTokens)
}

// observe counts and times every request, and writes its access log line
// when the log is on. It runs inside assignRequestIDs and the path aliases,
// so it sees the request's ID and canonical path; the route is the pattern
// the mux matches, so the metrics' labels stay bounded whatever paths
// clients send.
func (s *server) observe(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		_, pattern := mux.Handler(r)
		route := routeLabel(pattern)
		observation := &requestObservation{}
		if id := requestIDFrom(r.Context()); id != "" {
			observations.Store(id, observation)
			defer observations.Delete(id)
		}
		var body *countingBody
		if r.Body != nil && r.Body != http.NoBody {
			body = &countingBody{ReadCloser: r.Body}
			r.Body = body
		}
		recorder := &statusRecorder{ResponseWriter: w}
		s.metrics.inFlight.Add(1)
		defer s.metrics.inFlight.Add(-1)
		next.ServeHTTP(recorder, r.WithContext(context.WithValue(r.Context(), observationKey{}, observation)))

		elapsed := time.Since(started)
		status := recorder.statusCode()
		observation.mu.Lock()
		seen := observation.observed
		observation.mu.Unlock()
		s.metrics.observeRequest(route, r.Method, status, elapsed)
		s.metrics.observeUpstream(seen.provider, seen.model, seen.upstreamStatus, seen.inputTokens, seen.outputTokens)
		if s.accessLog == nil {
			return
		}
		attrs := []slog.Attr{
			slog.String("request_id", requestIDFrom(r.Context())),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Float64("duration_ms", float64(elapsed.Microseconds())/1000),
			slog.Int64("bytes_out", recorder.written),
		}
		if body != nil {
			attrs = append(attrs, slog.Int64("bytes_in", body.read))
		}
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			attrs = append(attrs, slog.String("remote_ip", host))
		}
		if agent := r.UserAgent(); agent != "" {
			attrs = append(attrs, slog.String("user_agent", truncateRunes(agent, 200)))
		}
		for _, field := range []struct{ name, value string }{
			{"caller", seen.caller}, {"project_id", seen.projectID}, {"key_id", seen.keyID},
			{"key_fingerprint", seen.keyFingerprint}, {"provider", seen.provider}, {"model", seen.model},
		} {
			if field.value != "" {
				attrs = append(attrs, slog.String(field.name, field.value))
			}
		}
		if seen.inputTokens > 0 || seen.outputTokens > 0 {
			attrs = append(attrs, slog.Int64("input_tokens", seen.inputTokens), slog.Int64("output_tokens", seen.outputTokens))
		}
		s.accessLog.LogAttrs(context.Background(), slog.LevelInfo, "request", attrs...)
	})
}

// routeLabel is the path of a mux pattern, such as /v1/chat/completions or
// /admin/api/providers/{id}, or "unmatched" for a request no route serves.
func routeLabel(pattern string) string {
	if pattern == "" {
		return "unmatched"
	}
	if _, path, ok := strings.Cut(pattern, " "); ok {
		return path
	}
	return pattern
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// accessLogger returns the access log LLMGW_ACCESS_LOG asks for: one JSON
// object per request on out, or nil when the log is off, as it is by
// default. A request's line names its caller by kind, project, key ID or a
// static key's fingerprint, never by a credential, and carries no query
// string, header other than the user agent, or body.
func accessLogger(out io.Writer) *slog.Logger {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_ACCESS_LOG"))) {
	case "1", "true", "yes", "on", "json":
		return slog.New(slog.NewJSONHandler(out, nil))
	}
	return nil
}

// countingBody counts the request body bytes a handler reads.
type countingBody struct {
	io.ReadCloser
	read int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}

// statusRecorder remembers a response's status and size, and passes
// flushes through so streams are not buffered.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.written += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() { _ = s.FlushError() }

func (s *statusRecorder) FlushError() error {
	return http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// statusCode is the status the response was sent with; a handler that wrote
// nothing sent 200.
func (s *statusRecorder) statusCode() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}
