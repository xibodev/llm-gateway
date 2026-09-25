package providers

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"

	"github.com/gorilla/websocket"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

type trackingReadCloser struct {
	closeCount int
}

func (*trackingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (body *trackingReadCloser) Close() error {
	body.closeCount++
	return nil
}

// edgeTTSAudioFrame is a binary audio message: the two-byte big-endian
// length of its headers, the headers, then the audio.
func edgeTTSAudioFrame(audio string) []byte {
	headers := "X-RequestId:abc\r\nContent-Type:audio/mpeg\r\nPath:audio\r\n"
	frame := make([]byte, 2, 2+len(headers)+len(audio))
	binary.BigEndian.PutUint16(frame, uint16(len(headers)))
	return append(append(frame, headers...), audio...)
}

// edgeTTSHandshake is how the fake service answers one dial: a refusal
// with its response, or, with a nil err, a connection.
type edgeTTSHandshake struct {
	response *http.Response
	err      error
}

// edgeTTSFake is an in-memory Edge TTS service behind core's dialer seam.
// Each dial takes the next queued handshake and accepts once none is left.
// An accepted connection records the text frames written to it and answers
// with the audio and turn.end, or, when broken, with a failed read.
type edgeTTSFake struct {
	mu         sync.Mutex
	handshakes []edgeTTSHandshake
	urls       []string
	frames     [][]string
	audio      string
	broken     bool
}

func (f *edgeTTSFake) dial(_ context.Context, rawURL string, _ http.Header, subprotocols []string) (coreproviders.WebSocketConn, *http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, rawURL)
	if len(subprotocols) != 1 || subprotocols[0] != "synthesize" {
		return nil, nil, errors.New("the synthesize subprotocol was not offered")
	}
	if len(f.handshakes) > 0 {
		handshake := f.handshakes[0]
		f.handshakes = f.handshakes[1:]
		if handshake.err != nil {
			return nil, handshake.response, handshake.err
		}
	}
	f.frames = append(f.frames, nil)
	return &edgeTTSFakeConn{fake: f, index: len(f.frames) - 1}, nil, nil
}

type edgeTTSFakeConn struct {
	fake  *edgeTTSFake
	index int
	reads int
}

func (c *edgeTTSFakeConn) WriteText(_ context.Context, data []byte) error {
	c.fake.mu.Lock()
	defer c.fake.mu.Unlock()
	c.fake.frames[c.index] = append(c.fake.frames[c.index], string(data))
	return nil
}

func (c *edgeTTSFakeConn) Read(context.Context) (int, []byte, error) {
	c.reads++
	switch {
	case c.fake.broken:
		return 0, nil, io.ErrUnexpectedEOF
	case c.reads == 1:
		return coreproviders.WebSocketBinaryMessage, edgeTTSAudioFrame(c.fake.audio), nil
	case c.reads == 2:
		return coreproviders.WebSocketTextMessage, []byte("X-RequestId:abc\r\nPath:turn.end\r\n\r\n{}"), nil
	}
	return 0, nil, io.ErrUnexpectedEOF
}

func (c *edgeTTSFakeConn) Close() error { return nil }

// edgeTTSFixture returns the provider at base over the fake service, with a
// clock stopped at now.
func edgeTTSFixture(t *testing.T, fake *edgeTTSFake, base, token, voice string, now time.Time) *EdgeTTSProvider {
	t.Helper()
	provider, err := newEdgeTTS(coreproviders.EdgeTTSConfig{
		Dial: fake.dial, BaseURL: base, DefaultVoice: voice, Timeout: time.Second,
		Now: func() time.Time { return now },
	}, token)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// Every dial is signed: the access token, a Sec-MS-GEC signature of upper-
// case SHA-256 hex, its version and a connection ID. An explicit http://
// base opts into plaintext; any other uses TLS.
func TestNewEdgeTTSDefaultsAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		base, token, voice      string
		scheme, host, path      string
		wantVoice, wantOverride string
	}{
		{scheme: "wss", path: "/tts/cognitiveservices/websocket/v1", wantVoice: "en-US-EmmaMultilingualNeural"},
		{base: "https://relay.example.com/tts/", token: " custom-token ", voice: "en-GB-SoniaNeural",
			scheme: "wss", host: "relay.example.com", path: "/tts/websocket/v1", wantVoice: "en-GB-SoniaNeural", wantOverride: "custom-token"},
		{base: "http://127.0.0.1:9999/base", scheme: "ws", host: "127.0.0.1:9999", path: "/base/websocket/v1", wantVoice: "en-US-EmmaMultilingualNeural"},
	} {
		fake := &edgeTTSFake{audio: "MP3"}
		provider := edgeTTSFixture(t, fake, tc.base, tc.token, tc.voice, time.Now())
		if _, _, err := provider.Synthesize("", "hi", ""); err != nil || provider.DefaultVoice() != tc.wantVoice {
			t.Fatalf("%q: err=%v default voice=%q", tc.base, err, provider.DefaultVoice())
		}
		dialed, err := url.Parse(fake.urls[0])
		if err != nil || dialed.Scheme != tc.scheme || tc.host != "" && dialed.Host != tc.host || dialed.Host == "" || dialed.Path != tc.path {
			t.Fatalf("%q: dialed %s", tc.base, fake.urls[0])
		}
		query := dialed.Query()
		if key := query.Get("Ocp-Apim-Subscription-Key"); key == "" || tc.wantOverride != "" && key != tc.wantOverride || tc.wantOverride == "" && key == "custom-token" {
			t.Fatalf("%q: access token %q", tc.base, key)
		}
		if !regexp.MustCompile(`^[0-9A-F]{64}$`).MatchString(query.Get("Sec-MS-GEC")) || query.Get("Sec-MS-GEC-Version") == "" || query.Get("ConnectionId") == "" {
			t.Fatalf("%q: unsigned dial %s", tc.base, fake.urls[0])
		}
		if ssml := fake.frames[0][1]; !strings.Contains(ssml, "<voice name='"+tc.wantVoice+"'>") || !strings.Contains(ssml, "rate='+0%'") {
			t.Fatalf("%q: ssml %q", tc.base, ssml)
		}
	}
}

// Long text is escaped and spoken in chunks of at most 4096 bytes, each over
// its own connection, split at whitespace and never inside an entity; the
// audio of the chunks is joined.
func TestEdgeTTSSpeaksLongTextInChunks(t *testing.T) {
	fake := &edgeTTSFake{audio: "MP3|"}
	provider := edgeTTSFixture(t, fake, "", "", "", time.Now())
	text := strings.Repeat("hello world ", 400) + "you & me" + strings.Repeat(" tail text", 300)
	audio, format, err := provider.Synthesize("en-US-TestNeural", text, "-25%")
	if err != nil || format != coreproviders.EdgeTTSOutputFormat {
		t.Fatalf("format=%q err=%v", format, err)
	}
	var spoken []string
	for _, frames := range fake.frames {
		ssml := frames[1]
		start := strings.Index(ssml, "volume='+0%'>") + len("volume='+0%'>")
		chunk := ssml[start:strings.Index(ssml, "</prosody>")]
		if len(chunk) > 4096 || !strings.Contains(ssml, "rate='-25%'") {
			t.Fatalf("chunk of %d bytes in %q", len(chunk), ssml[:200])
		}
		spoken = append(spoken, chunk)
	}
	if len(spoken) < 2 || string(audio) != strings.Repeat("MP3|", len(spoken)) {
		t.Fatalf("chunks=%d audio=%q", len(spoken), audio)
	}
	if joined := strings.Join(spoken, " "); !strings.Contains(joined, "you &amp; me") || joined != strings.ReplaceAll(strings.TrimSpace(text), "&", "&amp;") {
		t.Fatal("chunks did not rebuild the escaped text")
	}
}

func TestEdgeTTSCompleteRefusesChat(t *testing.T) {
	provider, err := NewEdgeTTS("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Complete("voice", nil, nil); err == nil || !strings.Contains(err.Error(), "/v1/audio/speech") {
		t.Fatalf("Complete should redirect to the speech endpoint, got %v", err)
	}
}

// mockEdgeTTSService speaks the Edge read-aloud websocket protocol: it expects
// speech.config and ssml text frames, then streams binary audio frames and a
// turn.end marker. A silent service upgrades and then never answers.
func mockEdgeTTSService(t *testing.T, audioPayload []byte, silent bool) (*httptest.Server, *string) {
	t.Helper()
	upgrader := websocket.Upgrader{
		Subprotocols: []string{"synthesize"},
		// The real service accepts the extension origin the protocol sends.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	var receivedSSML string
	mux := http.NewServeMux()
	mux.HandleFunc("/tts/voices/list", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("Ocp-Apim-Subscription-Key") == "" {
			http.Error(w, "missing key", http.StatusUnauthorized)
			return
		}
		// The real service rejects unsigned voice-list requests with 403.
		if r.URL.Query().Get("Sec-MS-GEC") == "" || r.URL.Query().Get("Sec-MS-GEC-Version") == "" {
			http.Error(w, "missing request signature", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"ShortName": "en-US-TestNeural", "FriendlyName": "Test voice", "Locale": "en-US", "Gender": "Female"},
		})
	})
	mux.HandleFunc("/tts/websocket/v1", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("Ocp-Apim-Subscription-Key") == "" || query.Get("Sec-MS-GEC") == "" || query.Get("ConnectionId") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		connection, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer connection.Close()
		// Never let a failed exchange wedge httptest.Server.Close.
		_ = connection.SetReadDeadline(time.Now().Add(10 * time.Second))
		// speech.config
		_, config, err := connection.ReadMessage()
		if err != nil || !strings.Contains(string(config), "Path:speech.config") {
			t.Errorf("expected speech.config, got %q err=%v", config, err)
			return
		}
		// ssml
		_, ssml, err := connection.ReadMessage()
		if err != nil || !strings.Contains(string(ssml), "Path:ssml") {
			t.Errorf("expected ssml, got %q err=%v", ssml, err)
			return
		}
		receivedSSML = string(ssml)
		if silent {
			// Wait for the client to give up and close the connection.
			_, _, _ = connection.ReadMessage()
			return
		}
		if err := connection.WriteMessage(websocket.BinaryMessage, edgeTTSAudioFrame(string(audioPayload))); err != nil {
			t.Errorf("write audio: %v", err)
			return
		}
		end := "X-RequestId:abc\r\nPath:turn.end\r\n\r\n{}"
		if err := connection.WriteMessage(websocket.TextMessage, []byte(end)); err != nil {
			t.Errorf("write turn.end: %v", err)
		}
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &receivedSSML
}

// The gorilla dialer and connection carry core's Edge TTS end to end.
func TestEdgeTTSSynthesizeAgainstMockService(t *testing.T) {
	wantAudio := []byte("FAKE-MP3-BYTES")
	server, receivedSSML := mockEdgeTTSService(t, wantAudio, false)
	base := strings.TrimPrefix(server.URL, "http://") + "/tts"
	provider, err := NewEdgeTTS("http://"+base, "test-token", "en-US-TestNeural", 10)
	if err != nil {
		t.Fatal(err)
	}

	audio, format, err := provider.Synthesize("", "Hello <world> & friends", "+10%")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != string(wantAudio) {
		t.Fatalf("audio = %q, want %q", audio, wantAudio)
	}
	if format != "audio-24khz-48kbitrate-mono-mp3" {
		t.Fatalf("format = %q", format)
	}
	if !strings.Contains(*receivedSSML, "en-US-TestNeural") || !strings.Contains(*receivedSSML, "rate='+10%'") {
		t.Fatalf("ssml missing voice or rate: %q", *receivedSSML)
	}
	if !strings.Contains(*receivedSSML, "Hello &lt;world&gt; &amp; friends") {
		t.Fatalf("ssml did not escape input: %q", *receivedSSML)
	}

	models := provider.ListModels()
	if len(models) != 1 || models[0].ID != "en-US-TestNeural" || models[0].Vendor != "microsoft" || models[0].Label != "Test voice" {
		t.Fatalf("voice catalog: %+v", models)
	}
	if surfaces := models[0].SupportedSurfaces; len(surfaces) != 1 || surfaces[0] != "/v1/audio/speech" {
		t.Fatalf("supported surfaces: %+v", surfaces)
	}
	if capabilities := models[0].Capabilities; capabilities["tts"] != true || capabilities["audio"] != true {
		t.Fatalf("capabilities: %+v", capabilities)
	}
}

// A read waits no longer than the exchange's deadline: a service that never
// answers fails the synthesis at the timeout, as a websocket that broke.
func TestEdgeTTSExchangeEndsAtTheTimeout(t *testing.T) {
	server, _ := mockEdgeTTSService(t, nil, true)
	provider, err := NewEdgeTTS("http://"+strings.TrimPrefix(server.URL, "http://")+"/tts", "test-token", "", 0.2)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, _, err = provider.Synthesize("", "hello", "")
	if !IsInvocation(err) || !InvocationRetryable(err) || UpstreamStatus(err) != 0 || time.Since(started) > 5*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(started))
	}
}

// A refused handshake keeps its status, closes the response the dialer
// returned, and leaks neither the token, the signature, the connection ID
// nor the host; the Retry-After the client never read stays unread.
func TestEdgeTTSHandshakeRefusalsAreClosedAndSafe(t *testing.T) {
	const (
		subscriptionKey = "llmgw_edge_subscription_secret_123456"
		gecSignature    = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
		connectionID    = "0123456789abcdef0123456789abcdef"
	)
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &trackingReadCloser{}
			refusal := errors.New("websocket: bad handshake: https://speech.example.invalid/tts?Ocp-Apim-Subscription-Key=" +
				subscriptionKey + "&Sec-MS-GEC=" + gecSignature + "&ConnectionId=" + connectionID)
			fake := &edgeTTSFake{handshakes: []edgeTTSHandshake{{
				response: &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"7"}}, Body: body}, err: refusal,
			}}}
			provider := edgeTTSFixture(t, fake, "https://speech.example.invalid/tts", subscriptionKey, "", time.Now())

			_, _, err := provider.Synthesize("", "hello", "")
			if !IsInvocation(err) || UpstreamStatus(err) != status || InvocationRetryAfter(err) != "" {
				t.Fatalf("err=%v invocation=%v status=%d", err, IsInvocation(err), UpstreamStatus(err))
			}
			if len(fake.urls) != 1 || !strings.Contains(fake.urls[0], "Ocp-Apim-Subscription-Key="+subscriptionKey) {
				t.Fatalf("dials=%d, want one carrying the access token", len(fake.urls))
			}
			if body.closeCount != 1 {
				t.Fatalf("HTTP %d response body close count = %d, want 1", status, body.closeCount)
			}
			message := err.Error()
			for _, secret := range []string{subscriptionKey, gecSignature, connectionID, "speech.example.invalid", "Ocp-Apim-Subscription-Key", "Sec-MS-GEC", "ConnectionId"} {
				if strings.Contains(message, secret) {
					t.Fatalf("error leaked %q: %q", secret, message)
				}
			}
		})
	}
}

// A handshake refused with 403 teaches the provider's core instance the
// service's clock from its Date, and the dial is repeated once with a new
// signature and connection ID. The provider keeps that instance, so its
// next request is signed with the learned clock and needs no refusal.
func TestEdgeTTSLearnsTheClockSkewOnceAndKeepsIt(t *testing.T) {
	now := time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)
	forbidden := func(body io.ReadCloser, date time.Time) edgeTTSHandshake {
		return edgeTTSHandshake{
			response: &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Date": {date.Format(time.RFC1123)}}, Body: body},
			err:      errors.New("websocket: bad handshake"),
		}
	}
	t.Run("success", func(t *testing.T) {
		firstBody := &trackingReadCloser{}
		fake := &edgeTTSFake{audio: "MP3", handshakes: []edgeTTSHandshake{forbidden(firstBody, now.Add(time.Hour))}}
		provider := edgeTTSFixture(t, fake, "https://speech.example.invalid/tts", "test-token", "", now)
		for request := 0; request < 2; request++ {
			if _, _, err := provider.Synthesize("", "hello", ""); err != nil {
				t.Fatalf("request %d: %v", request, err)
			}
		}
		if len(fake.urls) != 3 || firstBody.closeCount != 1 {
			t.Fatalf("dials=%d first body closes=%d, want 3 and 1", len(fake.urls), firstBody.closeCount)
		}
		queries := make([]url.Values, len(fake.urls))
		for index, rawURL := range fake.urls {
			parsed, err := url.Parse(rawURL)
			if err != nil {
				t.Fatalf("parse dial URL %d: %v", index+1, err)
			}
			queries[index] = parsed.Query()
		}
		if queries[0].Get("Sec-MS-GEC") == queries[1].Get("Sec-MS-GEC") || queries[0].Get("ConnectionId") == queries[1].Get("ConnectionId") {
			t.Fatalf("retry was not signed anew: %q", fake.urls[:2])
		}
		if queries[2].Get("Sec-MS-GEC") != queries[1].Get("Sec-MS-GEC") {
			t.Fatal("the next request did not keep the learned clock skew")
		}
		for _, key := range []string{"Ocp-Apim-Subscription-Key", "Sec-MS-GEC-Version"} {
			if queries[0].Get(key) == "" || queries[0].Get(key) != queries[1].Get(key) || queries[1].Get(key) != queries[2].Get(key) {
				t.Fatalf("stable signing input %s changed across dials", key)
			}
		}
	})

	t.Run("final forbidden", func(t *testing.T) {
		bodies := []*trackingReadCloser{{}, {}}
		fake := &edgeTTSFake{handshakes: []edgeTTSHandshake{forbidden(bodies[0], now), forbidden(bodies[1], now)}}
		provider := edgeTTSFixture(t, fake, "https://speech.example.invalid/tts", "secret-token", "", now)
		_, _, err := provider.Synthesize("", "hello", "")
		if UpstreamStatus(err) != http.StatusForbidden || InvocationRetryable(err) {
			t.Fatalf("err=%v status=%d", err, UpstreamStatus(err))
		}
		if len(fake.urls) != 2 || bodies[0].closeCount != 1 || bodies[1].closeCount != 1 {
			t.Fatalf("dials=%d body close counts=%d,%d; want 2 and 1,1", len(fake.urls), bodies[0].closeCount, bodies[1].closeCount)
		}
	})
}

// A websocket that cannot be opened, or that breaks, may be repeated, and
// the error quotes nothing of the dial, whose URL carries the token.
func TestEdgeTTSTransportFailuresAreGeneric(t *testing.T) {
	fake := &edgeTTSFake{handshakes: []edgeTTSHandshake{{err: errors.New("transport failed for wss://speech.example.invalid/tts?Ocp-Apim-Subscription-Key=secret-token")}}}
	provider := edgeTTSFixture(t, fake, "https://speech.example.invalid/tts", "secret-token", "", time.Now())
	_, _, err := provider.Synthesize("", "hello", "")
	if !IsInvocation(err) || UpstreamStatus(err) != 0 || !InvocationRetryable(err) {
		t.Fatalf("dial error = %v, invocation=%v status=%d", err, IsInvocation(err), UpstreamStatus(err))
	}
	if got, want := err.Error(), "edge_tts: Edge TTS could not open its websocket"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
	fake.broken = true
	if _, _, err := provider.Synthesize("", "hello", ""); !InvocationRetryable(err) || !InvocationCircuitFailure(err) || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("broken socket: err=%v", err)
	}
}

// What core refuses before anything is sent fails as the client failed it:
// text with nothing to speak is a plain invocation failure, an access token
// the URLs cannot carry a configuration error, and a canceled request its
// context's error.
func TestEdgeTTSRefusalsBeforeSending(t *testing.T) {
	fake := &edgeTTSFake{audio: "MP3"}
	provider := edgeTTSFixture(t, fake, "", "", "", time.Now())
	if _, _, err := provider.Synthesize("", "\x01\x02", ""); !IsInvocation(err) || UpstreamStatus(err) != 0 || InvocationRetryable(err) || InvocationCircuitFailure(err) {
		t.Fatalf("empty text: err=%v", err)
	}
	if _, _, err := edgeTTSFixture(t, fake, "", "not a token", "", time.Now()).Synthesize("", "hello", ""); !IsConfig(err) {
		t.Fatalf("unusable token: err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := provider.SynthesizeContext(ctx, "", "hello", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: err=%v", err)
	}
	if len(fake.urls) != 0 {
		t.Fatalf("refused requests dialed %d times", len(fake.urls))
	}
	if _, err := NewEdgeTTS("https://relay.example.com/tts?query=1", "", "", 0); !IsConfig(err) {
		t.Fatalf("a base with a query built a provider: err=%v", err)
	}
}

// The provider cache keeps one provider per instance and caller, so its core
// instance, and the clock skew that instance learns, serve every request.
func TestEdgeTTSProviderIsCachedPerInstance(t *testing.T) {
	runtime := InstallForTests(t)
	old := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"edge": {Type: "edge_tts"}}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = old }) })
	first, err := runtime.GetProvider("edge")
	if err != nil {
		t.Fatal(err)
	}
	second, err := runtime.GetProvider("edge")
	if err != nil || first != second {
		t.Fatalf("the provider was rebuilt: err=%v", err)
	}
	if _, ok := AsSpeechSynthesizer(first); !ok {
		t.Fatalf("provider=%T, want a speech synthesizer", first)
	}
}

func TestAsSpeechSynthesizerUnwrapsResilientDecorator(t *testing.T) {
	inner, err := NewEdgeTTS("", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	wrapped := &ResilientProvider{inner: inner, name: "edge_tts"}
	synthesizer, ok := AsSpeechSynthesizer(wrapped)
	if !ok {
		t.Fatalf("wrapped edge_tts not detected as speech synthesizer")
	}
	if synthesizer.DefaultVoice() != "en-US-EmmaMultilingualNeural" {
		t.Fatalf("unwrapped synthesizer lost configuration")
	}
	if _, ok := AsSpeechSynthesizer(EchoProvider{}); ok {
		t.Fatalf("echo must not report speech capability")
	}
}
