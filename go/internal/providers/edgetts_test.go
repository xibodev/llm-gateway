package providers

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

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
