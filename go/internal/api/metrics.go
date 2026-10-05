package api

import (
	"crypto/subtle"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgw/internal/buildinfo"
)

// durationBuckets are the upper bounds, in seconds, of the request duration
// histogram: from a health check to a long generation.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// gatewayMetrics are the counters /metrics exposes in the Prometheus text
// format. Their labels are routes the mux registered and the providers and
// models that served requests, never a path or model name a client sent
// unchecked, so the series stay bounded.
type gatewayMetrics struct {
	started  time.Time
	inFlight atomic.Int64

	mu        sync.Mutex
	requests  map[[3]string]uint64  // route, method, code
	durations map[string]*histogram // route
	upstream  map[[3]string]uint64  // provider, model, outcome
	tokens    map[[3]string]uint64  // provider, model, direction
}

type histogram struct {
	buckets []uint64 // per bound, not cumulative
	count   uint64
	sum     float64
}

func newGatewayMetrics(now time.Time) *gatewayMetrics {
	return &gatewayMetrics{
		started:   now,
		requests:  map[[3]string]uint64{},
		durations: map[string]*histogram{},
		upstream:  map[[3]string]uint64{},
		tokens:    map[[3]string]uint64{},
	}
}

func (m *gatewayMetrics) observeRequest(route, method string, status int, elapsed time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[3]string{route, method, strconv.Itoa(status)}]++
	h := m.durations[route]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(durationBuckets))}
		m.durations[route] = h
	}
	seconds := elapsed.Seconds()
	if i, _ := slices.BinarySearch(durationBuckets, seconds); i < len(durationBuckets) {
		h.buckets[i]++
	}
	h.count++
	h.sum += seconds
}

// observeUpstream counts a request a provider served or refused, and its
// tokens. A request that reached no provider counts nothing here.
func (m *gatewayMetrics) observeUpstream(provider, model string, status int, input, output int64) {
	if provider == "" {
		return
	}
	outcome := "success"
	if status >= http.StatusBadRequest {
		outcome = "error"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstream[[3]string{provider, model, outcome}]++
	if input > 0 {
		m.tokens[[3]string{provider, model, "input"}] += uint64(input)
	}
	if output > 0 {
		m.tokens[[3]string{provider, model, "output"}] += uint64(output)
	}
}

// write renders every metric in the Prometheus text exposition format,
// series in a stable order.
func (m *gatewayMetrics) write(w io.Writer) {
	m.mu.Lock()
	requests, upstream, tokens := maps.Clone(m.requests), maps.Clone(m.upstream), maps.Clone(m.tokens)
	durations := make(map[string]histogram, len(m.durations))
	for route, h := range m.durations {
		durations[route] = histogram{buckets: slices.Clone(h.buckets), count: h.count, sum: h.sum}
	}
	m.mu.Unlock()

	build := buildinfo.Current()
	family(w, "llmgw_build_info", "gauge", "The running gateway's version and commit.")
	fmt.Fprintf(w, "llmgw_build_info{version=%s,commit=%s,goversion=%s} 1\n",
		quoteLabel(build.Version), quoteLabel(build.Commit), quoteLabel(runtime.Version()))

	family(w, "llmgw_http_requests_total", "counter", "HTTP requests the gateway answered, by route, method and status code.")
	for _, key := range sortedKeys(requests) {
		fmt.Fprintf(w, "llmgw_http_requests_total{route=%s,method=%s,code=%s} %d\n",
			quoteLabel(key[0]), quoteLabel(key[1]), quoteLabel(key[2]), requests[key])
	}

	family(w, "llmgw_http_request_duration_seconds", "histogram", "Time from receiving a request to its last byte, streams included, by route.")
	for _, route := range slices.Sorted(maps.Keys(durations)) {
		h := durations[route]
		cumulative := uint64(0)
		for i, bound := range durationBuckets {
			cumulative += h.buckets[i]
			fmt.Fprintf(w, "llmgw_http_request_duration_seconds_bucket{route=%s,le=%s} %d\n",
				quoteLabel(route), quoteLabel(strconv.FormatFloat(bound, 'g', -1, 64)), cumulative)
		}
		fmt.Fprintf(w, "llmgw_http_request_duration_seconds_bucket{route=%s,le=\"+Inf\"} %d\n", quoteLabel(route), h.count)
		fmt.Fprintf(w, "llmgw_http_request_duration_seconds_sum{route=%s} %s\n", quoteLabel(route), strconv.FormatFloat(h.sum, 'g', -1, 64))
		fmt.Fprintf(w, "llmgw_http_request_duration_seconds_count{route=%s} %d\n", quoteLabel(route), h.count)
	}

	family(w, "llmgw_http_requests_in_flight", "gauge", "Requests being served, streams included.")
	fmt.Fprintf(w, "llmgw_http_requests_in_flight %d\n", m.inFlight.Load())

	family(w, "llmgw_upstream_requests_total", "counter", "Requests a provider served or refused, by provider, model and outcome.")
	for _, key := range sortedKeys(upstream) {
		fmt.Fprintf(w, "llmgw_upstream_requests_total{provider=%s,model=%s,outcome=%s} %d\n",
			quoteLabel(key[0]), quoteLabel(key[1]), quoteLabel(key[2]), upstream[key])
	}

	family(w, "llmgw_tokens_total", "counter", "Tokens providers reported, by provider, model and direction.")
	for _, key := range sortedKeys(tokens) {
		fmt.Fprintf(w, "llmgw_tokens_total{provider=%s,model=%s,direction=%s} %d\n",
			quoteLabel(key[0]), quoteLabel(key[1]), quoteLabel(key[2]), tokens[key])
	}

	samples := []metrics.Sample{{Name: "/sched/goroutines:goroutines"}, {Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(samples)
	family(w, "go_goroutines", "gauge", "Goroutines that currently exist.")
	fmt.Fprintf(w, "go_goroutines %d\n", sampleUint(samples[0]))
	family(w, "go_memstats_heap_alloc_bytes", "gauge", "Bytes of heap objects allocated and not yet freed.")
	fmt.Fprintf(w, "go_memstats_heap_alloc_bytes %d\n", sampleUint(samples[1]))
	family(w, "process_start_time_seconds", "gauge", "When the process started, in seconds since the Unix epoch.")
	fmt.Fprintf(w, "process_start_time_seconds %d\n", m.started.Unix())
}

func family(w io.Writer, name, kind, help string) {
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func sampleUint(sample metrics.Sample) uint64 {
	if sample.Value.Kind() == metrics.KindUint64 {
		return sample.Value.Uint64()
	}
	return 0
}

func sortedKeys(series map[[3]string]uint64) [][3]string {
	return slices.SortedFunc(maps.Keys(series), func(a, b [3]string) int {
		return strings.Compare(a[0]+"\x00"+a[1]+"\x00"+a[2], b[0]+"\x00"+b[1]+"\x00"+b[2])
	})
}

// quoteLabel quotes a label value as the text format requires: backslash,
// double quote and line feed escaped.
func quoteLabel(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(value) + `"`
}

// handleMetrics serves the metrics to a scraper holding LLMGW_METRICS_TOKEN
// as a bearer token. Without the variable the endpoint does not exist, so a
// gateway exposes no operational data unless its operator asks for it.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(os.Getenv("LLMGW_METRICS_TOKEN"))
	if token == "" {
		http.NotFound(w, r)
		return
	}
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(presented)), []byte(token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	s.metrics.write(w)
}
