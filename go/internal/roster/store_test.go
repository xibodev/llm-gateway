package roster

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, Payload) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, key, Payload{SchemaVersion: 1, Revision: 1, PublishedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), Entries: []Entry{{ID: "endpoint-test", Name: "Example", Protocol: "openai", BaseURL: "https://api.example.com/v1", Auth: "api_key", Offer: "unknown", Setup: "candidate", State: "active"}}, Sources: []Source{}}
}

func signed(t *testing.T, key ed25519.PrivateKey, p Payload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return signedBytes(t, key, b)
}

func signedBytes(t *testing.T, key ed25519.PrivateKey, b []byte) []byte {
	t.Helper()
	raw, err := json.Marshal(Envelope{KeyID: "staging", Payload: base64.StdEncoding.EncodeToString(b), Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, b))})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func clientFor(body *[]byte, calls *atomic.Int32) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(*body)), Header: make(http.Header)}, nil
	})}
}

func TestSignatureExactBytesAndValidation(t *testing.T) {
	pub, key, p := fixture(t)
	keys := map[string]ed25519.PublicKey{"staging": pub}
	b, _ := json.MarshalIndent(p, "", "  ")
	raw := signedBytes(t, key, b)
	if _, _, err := verify(raw, keys, time.Now()); err != nil {
		t.Fatal(err)
	}
	var env Envelope
	_ = json.Unmarshal(raw, &env)
	env.Payload = base64.StdEncoding.EncodeToString(append(b, ' '))
	tampered, _ := json.Marshal(env)
	if _, _, err := verify(tampered, keys, time.Now()); err == nil {
		t.Fatal("accepted tampered bytes")
	}
	if _, _, err := verify(raw, map[string]ed25519.PublicKey{"other": pub}, time.Now()); err == nil {
		t.Fatal("accepted unknown key id")
	}
	for _, change := range []func(*Payload){
		func(p *Payload) { p.SchemaVersion = 2 },
		func(p *Payload) { p.Revision = 1 << 53 },
		func(p *Payload) { p.PublishedAt = time.Now().Add(24 * time.Hour).Format(time.RFC3339) },
		func(p *Payload) { p.Entries[0].Auth = "oauth" },
		func(p *Payload) { p.Entries[0].Setup = "compatible"; p.Entries[0].Auth = "unknown" },
		func(p *Payload) { p.Entries[0].BaseURL = "https://example.com/?token=secret" },
		func(p *Payload) { p.Entries[0].Logo = &Logo{MIME: "image/svg+xml", Data: "PHN2Zz4="} },
	} {
		_, _, candidate := fixture(t)
		change(&candidate)
		if _, _, err := verify(signed(t, key, candidate), keys, time.Now()); err == nil {
			t.Fatal("accepted invalid signed payload")
		}
	}
}

func TestRefreshCacheRollbackAndStaleLastGood(t *testing.T) {
	pub, key, p := fixture(t)
	p.Revision = 2
	body := signed(t, key, p)
	var calls atomic.Int32
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("providers: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	o := Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, StateDir: dir, Client: clientFor(&body, &calls)}
	s := New(o)
	now := time.Now().Add(-time.Hour)
	s.clock = func() time.Time { return now }
	good := s.Refresh(context.Background())
	if good.Revision != 2 || good.Error != "" || good.Stale || good.LastSuccess == "" {
		t.Fatalf("bad snapshot: %+v", good)
	}
	good.Entries[0].Name = "caller mutation"
	if s.Snapshot().Entries[0].Name != "Example" {
		t.Fatal("snapshot aliases state")
	}
	cacheBefore, err := os.ReadFile(s.cachePath())
	if err != nil {
		t.Fatal(err)
	}
	reloaded := New(o)
	if reloaded.Snapshot().Revision != 2 {
		t.Fatal("cache did not reload")
	}
	older := p
	older.Revision = 1
	body = signed(t, key, older)
	if got := reloaded.Refresh(context.Background()); got.Revision != 2 || got.Error == "" {
		t.Fatalf("restart lost rollback protection: %+v", got)
	}
	for _, mode := range []string{"rollback", "equivocation", "publication rollback", "tamper"} {
		now = now.Add(refreshCooldown + time.Second)
		candidate := p
		switch mode {
		case "rollback":
			candidate.Revision = 1
		case "equivocation":
			candidate.PublishedAt = now.Add(-time.Minute).Format(time.RFC3339)
		case "publication rollback":
			candidate.Revision = 3
			candidate.PublishedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
		case "tamper":
		}
		body = signed(t, key, candidate)
		if mode == "tamper" {
			body[len(body)-5] ^= 1
		}
		got := s.Refresh(context.Background())
		if got.Revision != 2 || got.Error == "" || !got.Stale || got.LastSuccess != good.LastSuccess {
			t.Fatalf("%s replaced good roster: %+v", mode, got)
		}
		cacheAfter, _ := os.ReadFile(s.cachePath())
		if !bytes.Equal(cacheBefore, cacheAfter) {
			t.Fatal("rejected feed changed cache")
		}
	}
	p.Revision = 3
	body = signed(t, key, p)
	now = now.Add(refreshCooldown + time.Second)
	if got := s.Refresh(context.Background()); got.Revision != 3 || got.Error != "" {
		t.Fatalf("upgrade failed: %+v", got)
	}
	if New(o).Snapshot().Revision != 3 {
		t.Fatal("atomic cache replacement failed")
	}
	configAfter, _ := os.ReadFile(configPath)
	if string(configAfter) != "providers: {}\n" {
		t.Fatal("config changed")
	}
	if err := os.WriteFile(s.cachePath(), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := New(o).Snapshot(); got.Revision != 0 || got.Error == "" {
		t.Fatal("corrupt cache not fail-safe")
	}
}

func TestAutoOffManualCoalescingAndCooldown(t *testing.T) {
	pub, key, p := fixture(t)
	body := signed(t, key, p)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	s := New(Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, Client: c})
	stop := s.Start(context.Background())
	stop()
	stop()
	if calls.Load() != 0 {
		t.Fatal("auto off fetched")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.Refresh(context.Background()) }()
	<-entered
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Refresh(context.Background()) }()
	}
	close(release)
	wg.Wait()
	if got := s.Refresh(context.Background()); calls.Load() != 1 || got.Revision != 1 || got.AutoRefresh {
		t.Fatalf("manual/cooldown failed: %+v calls=%d", got, calls.Load())
	}
}

func TestEnvironmentAndStartupCancellation(t *testing.T) {
	pub, _, _ := fixture(t)
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_PROVIDER_ROSTER_URL", "")
	t.Setenv("LLMGW_PROVIDER_ROSTER_PUBLIC_KEY", "")
	t.Setenv("LLMGW_PROVIDER_ROSTER_KEY_ID", "")
	t.Setenv("LLMGW_PROVIDER_ROSTER_AUTO_REFRESH", "")
	if got := FromEnvironment().Snapshot(); got.Configured || !got.AutoRefresh {
		t.Fatalf("unsafe defaults: %+v", got)
	}
	t.Setenv("LLMGW_PROVIDER_ROSTER_URL", "https://feed.example.com/roster.json")
	if !FromEnvironment().Snapshot().Configured {
		t.Fatal("plain mode (URL without keys) should be configured")
	}
	t.Setenv("LLMGW_PROVIDER_ROSTER_PUBLIC_KEY", base64.StdEncoding.EncodeToString(pub))
	s := FromEnvironment()
	if !s.Snapshot().Configured || !bytes.Equal(s.options.Keys["staging"], pub) {
		t.Fatal("environment not applied")
	}
	entered := make(chan struct{})
	s.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})}
	stop := s.Start(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup blocked or did not fetch")
	}
	stop()
	if s.Snapshot().Error != "Roster fetch failed." {
		t.Fatal("cancellation not sanitized")
	}
	t.Setenv("LLMGW_PROVIDER_ROSTER_PUBLIC_KEY", "secret-invalid")
	if got := FromEnvironment().Snapshot(); got.Configured || strings.Contains(got.Error, "secret-invalid") {
		t.Fatal("invalid trust leaked or accepted")
	}
}

type fakeResolver []net.IPAddr

func (r fakeResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) { return r, nil }

func TestPublicNetworkBoundary(t *testing.T) {
	for _, raw := range []string{"http://example.com", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com?", "https://example.com/#", "https://localhost", "https://127.0.0.1", "https://10.0.0.1", "https://[::1]", "https://[::ffff:127.0.0.1]", "https://100.100.100.200", "https://example.local", "https://[2001:db8::1]", "https://example.com./"} {
		if _, err := publicURL(raw); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, ip := range []string{"0.0.0.0", "192.168.1.2", "169.254.169.254", "100.64.0.1", "224.0.0.1", "240.0.0.1", "fc00::1", "fe80::1", "64:ff9b::7f00:1", "2002:7f00:1::"} {
		if publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("accepted IP %s", ip)
		}
	}
	for _, ip := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(ip)) {
			t.Errorf("rejected public IP %s", ip)
		}
	}
	c := publicClient(fakeResolver{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}})
	_, err := c.Get("https://feed.example.com")
	if err == nil || !strings.Contains(err.Error(), "not public") {
		t.Fatalf("mixed DNS response not rejected before dialing: %v", err)
	}
}

func TestRedirectOversizeAndErrorSanitization(t *testing.T) {
	pub, _, _ := fixture(t)
	for _, kind := range []string{"redirect", "oversize", "secret"} {
		t.Run(kind, func(t *testing.T) {
			var calls atomic.Int32
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				switch kind {
				case "secret":
					return nil, errors.New("https://user:secret@example.com?token=secret")
				case "redirect":
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"https://127.0.0.1/secret"}}, Body: io.NopCloser(strings.NewReader("secret")), Request: r}, nil
				default:
					return &http.Response{StatusCode: 200, Body: io.NopCloser(io.LimitReader(zeroReader{}, maxFeedBytes+1))}, nil
				}
			})}
			s := New(Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, Client: c})
			got := s.Refresh(context.Background())
			s.Refresh(context.Background())
			if got.Error == "" || strings.Contains(got.Error, "secret") || calls.Load() != 1 {
				t.Fatalf("boundary failure: %+v calls=%d", got, calls.Load())
			}
		})
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestRasterLogo(t *testing.T) {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b.Bytes())
	l := Logo{MIME: "image/png", Data: base64.StdEncoding.EncodeToString(b.Bytes()), SHA256: hex.EncodeToString(h[:])}
	if !validLogo(&l) {
		t.Fatal("valid PNG rejected")
	}
	l.MIME = "image/jpeg"
	if validLogo(&l) {
		t.Fatal("MIME mismatch accepted")
	}
	l.MIME = "image/png"
	l.SHA256 = strings.Repeat("0", 64)
	if validLogo(&l) {
		t.Fatal("hash mismatch accepted")
	}
	truncated := b.Bytes()[:len(b.Bytes())-10]
	h = sha256.Sum256(truncated)
	l.Data = base64.StdEncoding.EncodeToString(truncated)
	l.SHA256 = hex.EncodeToString(h[:])
	if validLogo(&l) {
		t.Fatal("truncated image accepted")
	}
	l.Data = strings.Repeat("A", base64.StdEncoding.EncodedLen(128<<10)+4)
	if validLogo(&l) {
		t.Fatal("oversize logo accepted")
	}
}

func TestStaleFeedAndCacheWriteFailure(t *testing.T) {
	pub, key, p := fixture(t)
	p.PublishedAt = time.Now().Add(-96 * time.Hour).UTC().Format(time.RFC3339)
	body := signed(t, key, p)
	var calls atomic.Int32
	dir := t.TempDir()
	s := New(Options{URL: "https://feed.example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, StateDir: dir, Client: clientFor(&body, &calls)})
	now := time.Now()
	s.clock = func() time.Time { return now }
	if got := s.Refresh(context.Background()); got.Revision != 1 || !got.Stale || got.Error != "" {
		t.Fatalf("old authentic roster should remain usable but stale: %+v", got)
	}
	// A non-directory parent deterministically simulates an unavailable cache.
	blocked := filepath.Join(dir, "blocked")
	if err := os.WriteFile(blocked, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s.options.StateDir = blocked
	p.Revision = 2
	body = signed(t, key, p)
	now = now.Add(refreshCooldown + time.Second)
	if got := s.Refresh(context.Background()); got.Revision != 1 || got.Error != "Verified roster could not be saved to cache." {
		t.Fatalf("failed persistence changed high-water mark: %+v", got)
	}
}

func TestPinnedTLSFetchWithInjectedTransport(t *testing.T) {
	pub, key, p := fixture(t)
	body := signed(t, key, p)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	c := server.Client()
	transport := c.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	c.Transport = transport
	s := New(Options{URL: "https://example.com/roster.json", Keys: map[string]ed25519.PublicKey{"staging": pub}, Client: c})
	if got := s.Refresh(context.Background()); got.Error != "" || got.Revision != 1 {
		t.Fatalf("injected TLS server: %+v", got)
	}
}

func plainBody(t *testing.T, p Payload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPlainPayloadAcceptanceAndCache(t *testing.T) {
	_, _, p := fixture(t)
	p.Revision = 2
	body := plainBody(t, p)
	var calls atomic.Int32
	dir := t.TempDir()
	o := Options{URL: "https://feed.example.com/roster.json", StateDir: dir, Client: clientFor(&body, &calls)}
	s := New(o)
	if !s.Snapshot().Configured {
		t.Fatal("plain mode should be configured with URL only")
	}
	now := time.Now().Add(-time.Hour)
	s.clock = func() time.Time { return now }
	good := s.Refresh(context.Background())
	if good.Revision != 2 || good.Error != "" || good.Stale || good.LastSuccess == "" {
		t.Fatalf("plain refresh failed: %+v", good)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected 1 fetch, got %d", calls.Load())
	}
	// Reload from cache
	reloaded := New(o)
	if reloaded.Snapshot().Revision != 2 {
		t.Fatal("plain cache did not reload")
	}
	// Rollback rejection works in plain mode too
	older := p
	older.Revision = 1
	body = plainBody(t, older)
	if got := reloaded.Refresh(context.Background()); got.Revision != 2 || got.Error == "" {
		t.Fatalf("plain mode lost rollback protection: %+v", got)
	}
}

func TestPlainModeRejectsMalformedJSON(t *testing.T) {
	for _, raw := range []string{
		"not json",
		`{"schema_version":2,"revision":1,"published_at":"2026-01-01T00:00:00Z","entries":[],"sources":[]}`,
		string(bytes.Repeat([]byte("x"), maxFeedBytes+1)),
	} {
		body := []byte(raw)
		var calls atomic.Int32
		s := New(Options{URL: "https://feed.example.com/roster.json", Client: clientFor(&body, &calls)})
		if got := s.Refresh(context.Background()); got.Error == "" {
			t.Errorf("accepted malformed plain payload: %s", raw[:min(len(raw), 50)])
		}
	}
}

func TestPlainModeRollbackEquivocationAndUpgrade(t *testing.T) {
	_, _, p := fixture(t)
	p.Revision = 2
	body := plainBody(t, p)
	var calls atomic.Int32
	dir := t.TempDir()
	o := Options{URL: "https://feed.example.com/roster.json", StateDir: dir, Client: clientFor(&body, &calls)}
	s := New(o)
	now := time.Now().Add(-time.Hour)
	s.clock = func() time.Time { return now }
	good := s.Refresh(context.Background())
	if good.Revision != 2 || good.Error != "" {
		t.Fatalf("initial plain refresh: %+v", good)
	}
	for _, mode := range []string{"rollback", "equivocation", "publication rollback"} {
		now = now.Add(refreshCooldown + time.Second)
		candidate := p
		switch mode {
		case "rollback":
			candidate.Revision = 1
		case "equivocation":
			candidate.PublishedAt = now.Add(-time.Minute).Format(time.RFC3339)
		case "publication rollback":
			candidate.Revision = 3
			candidate.PublishedAt = now.Add(-24 * time.Hour).Format(time.RFC3339)
		}
		body = plainBody(t, candidate)
		got := s.Refresh(context.Background())
		if got.Revision != 2 || got.Error == "" {
			t.Fatalf("plain %s replaced good roster: %+v", mode, got)
		}
	}
	// Upgrade works
	p.Revision = 3
	body = plainBody(t, p)
	now = now.Add(refreshCooldown + time.Second)
	if got := s.Refresh(context.Background()); got.Revision != 3 || got.Error != "" {
		t.Fatalf("plain upgrade failed: %+v", got)
	}
}

func TestPlainModeCoalescingAndCooldown(t *testing.T) {
	_, _, p := fixture(t)
	body := plainBody(t, p)
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	s := New(Options{URL: "https://feed.example.com/roster.json", Client: c})
	stop := s.Start(context.Background())
	stop()
	if calls.Load() != 0 {
		t.Fatal("auto off fetched")
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.Refresh(context.Background()) }()
	<-entered
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Refresh(context.Background()) }()
	}
	close(release)
	wg.Wait()
	if got := s.Refresh(context.Background()); calls.Load() != 1 || got.Revision != 1 || got.AutoRefresh {
		t.Fatalf("plain manual/cooldown failed: %+v calls=%d", got, calls.Load())
	}
}
