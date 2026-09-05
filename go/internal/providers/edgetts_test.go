package providers

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type trackingReadCloser struct {
	closeCount int
}

func (*trackingReadCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (body *trackingReadCloser) Close() error {
	body.closeCount++
	return nil
}

func TestNewEdgeTTSDefaultsAndOverrides(t *testing.T) {
	defaults := NewEdgeTTS("", "", "", 0)
	if defaults.base != edgeTTSDefaultBase || defaults.token != edgeTTSDefaultToken || defaults.voice != edgeTTSDefaultVoice {
		t.Fatalf("defaults not applied: %+v", defaults)
	}
	if defaults.insecure {
		t.Fatalf("default transport must be TLS")
	}
	overridden := NewEdgeTTS("https://relay.example.com/tts/", "custom-token", "en-GB-SoniaNeural", 5)
	if overridden.base != "relay.example.com/tts" || overridden.token != "custom-token" || overridden.voice != "en-GB-SoniaNeural" {
		t.Fatalf("overrides not applied: %+v", overridden)
	}
	insecure := NewEdgeTTS("http://127.0.0.1:9999/base", "", "", 0)
	if !insecure.insecure || insecure.base != "127.0.0.1:9999/base" {
		t.Fatalf("insecure base not detected: %+v", insecure)
	}
}

func TestEdgeTTSSecurityTokenShape(t *testing.T) {
	provider := NewEdgeTTS("", "", "", 0)
	token := provider.securityToken()
	if !regexp.MustCompile(`^[0-9A-F]{64}$`).MatchString(token) {
		t.Fatalf("security token is not upper-hex sha256: %q", token)
	}
}

func TestEdgeTTSSplitPreservesEntitiesAndLength(t *testing.T) {
	text := strings.Repeat("hello world ", 40) + "&amp;" + strings.Repeat(" tail text", 30)
	chunks := edgeTTSSplit(text, 128)
	var rebuilt []string
	for _, chunk := range chunks {
		if len(chunk) > 128 {
			t.Fatalf("chunk exceeds limit: %d bytes", len(chunk))
		}
		if strings.Count(chunk, "&") != strings.Count(chunk, ";") && strings.Contains(chunk, "&") {
			// A chunk containing a bare & without its ; was split mid-entity.
			if !strings.Contains(chunk, "&amp;") {
				t.Fatalf("entity split across chunks: %q", chunk)
			}
		}
		rebuilt = append(rebuilt, chunk)
	}
	joined := strings.Join(rebuilt, " ")
	if !strings.Contains(joined, "&amp;") {
		t.Fatalf("entity lost during split")
	}
}

func TestEdgeTTSCompleteRefusesChat(t *testing.T) {
	provider := NewEdgeTTS("", "", "", 0)
	if _, err := provider.Complete("voice", nil, nil); err == nil || !strings.Contains(err.Error(), "/v1/audio/speech") {
		t.Fatalf("Complete should redirect to the speech endpoint, got %v", err)
	}
}

// mockEdgeTTSService speaks the Edge read-aloud websocket protocol: it expects
// speech.config and ssml text frames, then streams binary audio frames and a
// turn.end marker.
func mockEdgeTTSService(t *testing.T, audioPayload []byte) (*httptest.Server, *string) {
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
		// binary audio frame: 2-byte BE header length + headers + payload
		headers := []byte("X-RequestId:abc\r\nContent-Type:audio/mpeg\r\nPath:audio\r\n")
		frame := make([]byte, 2+len(headers)+len(audioPayload))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(headers)))
		copy(frame[2:], headers)
		copy(frame[2+len(headers):], audioPayload)
		if err := connection.WriteMessage(websocket.BinaryMessage, frame); err != nil {
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

func TestEdgeTTSSynthesizeAgainstMockService(t *testing.T) {
	wantAudio := []byte("FAKE-MP3-BYTES")
	server, receivedSSML := mockEdgeTTSService(t, wantAudio)
	base := strings.TrimPrefix(server.URL, "http://") + "/tts"
	provider := NewEdgeTTS("http://"+base, "test-token", "en-US-TestNeural", 10)

	audio, format, err := provider.Synthesize("", "Hello <world> & friends", "+10%")
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if string(audio) != string(wantAudio) {
		t.Fatalf("audio = %q, want %q", audio, wantAudio)
	}
	if format != edgeTTSOutputFormat {
		t.Fatalf("format = %q", format)
	}
	if !strings.Contains(*receivedSSML, "en-US-TestNeural") || !strings.Contains(*receivedSSML, "rate='+10%'") {
		t.Fatalf("ssml missing voice or rate: %q", *receivedSSML)
	}
	if !strings.Contains(*receivedSSML, "Hello &lt;world&gt; &amp; friends") {
		t.Fatalf("ssml did not escape input: %q", *receivedSSML)
	}

	models := provider.ListModels()
	if len(models) != 1 || models[0].ID != "en-US-TestNeural" {
		t.Fatalf("voice catalog: %+v", models)
	}
	if surfaces := models[0].SupportedSurfaces; len(surfaces) != 1 || surfaces[0] != "/v1/audio/speech" {
		t.Fatalf("supported surfaces: %+v", surfaces)
	}
}

func TestEdgeTTSDialHTTPFailuresAreClosedAndSafe(t *testing.T) {
	const (
		subscriptionKey = "llmgw_edge_subscription_secret_123456"
		gecSignature    = "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789"
		connectionID    = "0123456789abcdef0123456789abcdef"
	)
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &trackingReadCloser{}
			provider := NewEdgeTTS("https://speech.example.invalid/tts", subscriptionKey, "", 1)
			provider.dialWebsocket = func(rawURL string, _ http.Header) (*websocket.Conn, *http.Response, error) {
				if !strings.Contains(rawURL, "Ocp-Apim-Subscription-Key="+subscriptionKey) ||
					!strings.Contains(rawURL, "Sec-MS-GEC=") || !strings.Contains(rawURL, "ConnectionId=") {
					t.Fatalf("dial URL did not carry required protocol parameters")
				}
				return nil, &http.Response{StatusCode: status, Body: body}, errors.New(
					"websocket: bad handshake: " + rawURL + "&Sec-MS-GEC=" + gecSignature + "&ConnectionId=" + connectionID,
				)
			}

			_, err := provider.dial()
			if err == nil || !IsInvocation(err) || UpstreamStatus(err) != status {
				t.Fatalf("dial error = %v, invocation=%v status=%d", err, IsInvocation(err), UpstreamStatus(err))
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

func TestEdgeTTSDialRetriesForbiddenOnceAndClosesResponses(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		firstBody := &trackingReadCloser{}
		edgeTTSClockSkewMutex.Lock()
		originalSkew := edgeTTSClockSkewSeconds
		edgeTTSClockSkewSeconds = 0
		edgeTTSClockSkewMutex.Unlock()
		t.Cleanup(func() {
			edgeTTSClockSkewMutex.Lock()
			edgeTTSClockSkewSeconds = originalSkew
			edgeTTSClockSkewMutex.Unlock()
		})
		upgrader := websocket.Upgrader{
			Subprotocols: []string{"synthesize"},
			CheckOrigin:  func(*http.Request) bool { return true },
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			connection, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade: %v", err)
				return
			}
			defer connection.Close()
			_, _, _ = connection.ReadMessage()
		}))
		t.Cleanup(server.Close)
		provider := NewEdgeTTS(server.URL+"/tts", "test-token", "", 10)
		realDialer := websocket.Dialer{Subprotocols: []string{"synthesize"}}
		var dialURLs []string
		provider.dialWebsocket = func(rawURL string, header http.Header) (*websocket.Conn, *http.Response, error) {
			dialURLs = append(dialURLs, rawURL)
			if len(dialURLs) == 1 {
				return nil, &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     http.Header{"Date": []string{time.Now().UTC().Add(time.Hour).Format(time.RFC1123)}},
					Body:       firstBody,
				}, errors.New("websocket: bad handshake")
			}
			return realDialer.Dial(rawURL, header)
		}

		connection, err := provider.dial()
		if err != nil {
			t.Fatalf("dial after skew retry: %v", err)
		}
		connection.Close()
		if len(dialURLs) != 2 {
			t.Fatalf("dial attempts = %d, want 2", len(dialURLs))
		}
		if firstBody.closeCount != 1 {
			t.Fatalf("first response body close count = %d, want 1", firstBody.closeCount)
		}
		queries := make([]url.Values, len(dialURLs))
		for index, rawURL := range dialURLs {
			parsed, parseErr := url.Parse(rawURL)
			if parseErr != nil {
				t.Fatalf("parse dial URL %d: %v", index+1, parseErr)
			}
			queries[index] = parsed.Query()
			for _, key := range []string{"Ocp-Apim-Subscription-Key", "Sec-MS-GEC", "Sec-MS-GEC-Version", "ConnectionId"} {
				if queries[index].Get(key) == "" {
					t.Fatalf("dial URL %d missing %s", index+1, key)
				}
			}
		}
		if queries[0].Encode() == queries[1].Encode() {
			t.Fatalf("retry query was not rebuilt: %q", queries[0].Encode())
		}
		if queries[0].Get("Sec-MS-GEC") == queries[1].Get("Sec-MS-GEC") {
			t.Fatalf("retry Sec-MS-GEC did not reflect induced clock skew: %q", queries[0].Get("Sec-MS-GEC"))
		}
		if queries[0].Get("Ocp-Apim-Subscription-Key") != queries[1].Get("Ocp-Apim-Subscription-Key") ||
			queries[0].Get("Sec-MS-GEC-Version") != queries[1].Get("Sec-MS-GEC-Version") {
			t.Fatalf("stable signing inputs changed across retry")
		}
	})

	t.Run("final forbidden", func(t *testing.T) {
		bodies := []*trackingReadCloser{{}, {}}
		provider := NewEdgeTTS("https://speech.example.invalid/tts", "secret-token", "", 1)
		attempts := 0
		provider.dialWebsocket = func(string, http.Header) (*websocket.Conn, *http.Response, error) {
			body := bodies[attempts]
			attempts++
			return nil, &http.Response{StatusCode: http.StatusForbidden, Body: body}, errors.New("unsafe URL")
		}

		_, err := provider.dial()
		if err == nil || UpstreamStatus(err) != http.StatusForbidden {
			t.Fatalf("dial error = %v, status=%d", err, UpstreamStatus(err))
		}
		if attempts != 2 || bodies[0].closeCount != 1 || bodies[1].closeCount != 1 {
			t.Fatalf("attempts=%d body close counts=%d,%d; want 2 and 1,1", attempts, bodies[0].closeCount, bodies[1].closeCount)
		}
	})
}

func TestEdgeTTSDialTransportFailureIsGeneric(t *testing.T) {
	provider := NewEdgeTTS("https://speech.example.invalid/tts", "secret-token", "", 1)
	provider.dialWebsocket = func(rawURL string, _ http.Header) (*websocket.Conn, *http.Response, error) {
		return nil, nil, errors.New("transport failed for " + rawURL)
	}

	_, err := provider.dial()
	if err == nil || !IsInvocation(err) || UpstreamStatus(err) != 0 {
		t.Fatalf("dial error = %v, invocation=%v status=%d", err, IsInvocation(err), UpstreamStatus(err))
	}
	if got, want := err.Error(), "edge_tts: websocket transport failed"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestAsSpeechSynthesizerUnwrapsResilientDecorator(t *testing.T) {
	inner := NewEdgeTTS("", "", "", 0)
	wrapped := &ResilientProvider{inner: inner, name: "edge_tts"}
	synthesizer, ok := AsSpeechSynthesizer(wrapped)
	if !ok {
		t.Fatalf("wrapped edge_tts not detected as speech synthesizer")
	}
	if synthesizer.DefaultVoice() != edgeTTSDefaultVoice {
		t.Fatalf("unwrapped synthesizer lost configuration")
	}
	if _, ok := AsSpeechSynthesizer(EchoProvider{}); ok {
		t.Fatalf("echo must not report speech capability")
	}
}
