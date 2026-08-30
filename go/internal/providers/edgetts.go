package providers

// Native client for the Microsoft Edge read-aloud speech-synthesis service,
// exposed as gateway provider type "edge_tts".
//
// The wire protocol (endpoints, the well-known shared access token, the
// Sec-MS-GEC request signature, the speech.config/SSML websocket framing and
// the binary audio chunk format) is the publicly observable interface of
// Edge's read-aloud feature, as documented across the edge-tts ecosystem.
// This file implements that protocol from scratch so every operational
// parameter — base host, access token, default voice — has a baked-in
// default that can be overridden in provider configuration when the
// upstream service changes, without rebuilding the gateway.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	edgeTTSDefaultBase    = "api.msedgeservices.com/tts/cognitiveservices"
	edgeTTSDefaultToken   = "6A5AA1D4EAFF4E9FB37E23D68491D6F4"
	edgeTTSDefaultVoice   = "en-US-EmmaMultilingualNeural"
	edgeTTSChromiumFull   = "140.0.3485.14"
	edgeTTSChromiumMajor  = "140"
	edgeTTSOutputFormat   = "audio-24khz-48kbitrate-mono-mp3"
	edgeTTSWindowsEpoch   = 11644473600
	edgeTTSMaxMessageSize = 4096
)

var edgeTTSUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36" +
	" (KHTML, like Gecko) Chrome/" + edgeTTSChromiumMajor + ".0.0.0 Safari/537.36" +
	" Edg/" + edgeTTSChromiumMajor + ".0.0.0"

// edgeTTSClockSkew tracks the offset between the local clock and the service
// clock, learned from 403 responses that carry a server Date header.
var (
	edgeTTSClockSkewSeconds float64
	edgeTTSClockSkewMutex   sync.RWMutex
)

// SpeechSynthesizer is implemented by providers that produce audio from text.
// The audio speech endpoint and the verify operation prefer it over Complete.
type SpeechSynthesizer interface {
	Synthesize(voice, text, rate string) ([]byte, string, error)
	DefaultVoice() string
}

// EdgeTTSProvider synthesizes speech through the Edge read-aloud service.
type EdgeTTSProvider struct {
	base          string
	token         string
	voice         string
	insecure      bool
	timeout       time.Duration
	dialWebsocket func(string, http.Header) (*websocket.Conn, *http.Response, error)
}

// NewEdgeTTS builds the provider with baked-in defaults, applying any
// overrides from provider configuration. An explicit http:// or ws:// base
// opts into plaintext transport (local relays and tests); everything else
// uses TLS.
func NewEdgeTTS(baseURL, token, defaultVoice string, timeoutSeconds float64) EdgeTTSProvider {
	base := strings.TrimSpace(baseURL)
	insecure := strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "ws://")
	for _, prefix := range []string{"https://", "wss://", "http://", "ws://"} {
		base = strings.TrimPrefix(base, prefix)
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		base = edgeTTSDefaultBase
	}
	token = strings.TrimSpace(token)
	if token == "" {
		token = edgeTTSDefaultToken
	}
	voice := strings.TrimSpace(defaultVoice)
	if voice == "" {
		voice = edgeTTSDefaultVoice
	}
	timeout := time.Duration(timeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return EdgeTTSProvider{base: base, token: token, voice: voice, insecure: insecure, timeout: timeout}
}

func (p EdgeTTSProvider) httpScheme() string {
	if p.insecure {
		return "http://"
	}
	return "https://"
}

func (p EdgeTTSProvider) websocketScheme() string {
	if p.insecure {
		return "ws://"
	}
	return "wss://"
}

func (p EdgeTTSProvider) DefaultVoice() string { return p.voice }
func (p EdgeTTSProvider) IsStub() bool         { return false }

// Complete exists to satisfy the Provider interface; edge_tts has no chat API.
func (p EdgeTTSProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return nil, &ConfigError{Msg: "edge_tts synthesizes speech only — call POST /v1/audio/speech with this provider's voice as the model"}
}

func (p EdgeTTSProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: "edge_tts synthesizes speech only — call POST /v1/audio/speech with this provider's voice as the model"}
}

func (p EdgeTTSProvider) voicesURL() string {
	// The voices list requires the same Sec-MS-GEC request signature as the
	// synthesis websocket; without it the service answers 403.
	return p.httpScheme() + p.base + "/voices/list?Ocp-Apim-Subscription-Key=" + p.token +
		"&Sec-MS-GEC=" + p.securityToken() +
		"&Sec-MS-GEC-Version=1-" + edgeTTSChromiumFull
}

// ListModels exposes the service's voices as catalog models so voices appear
// in /v1/models, route building, and the provider detail page.
func (p EdgeTTSProvider) ListModels() []ModelInfo {
	client := &http.Client{Timeout: p.timeout}
	fetch := func() (*http.Response, error) {
		request, err := http.NewRequest(http.MethodGet, p.voicesURL(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("User-Agent", edgeTTSUserAgent)
		request.Header.Set("Accept", "*/*")
		return client.Do(request)
	}
	response, err := fetch()
	if err != nil {
		return nil
	}
	if response.StatusCode == http.StatusForbidden {
		// Learn clock skew from the server date and retry once, mirroring the
		// websocket dial path.
		if serverDate, parseErr := time.Parse(time.RFC1123, response.Header.Get("Date")); parseErr == nil {
			skew := float64(serverDate.UTC().Unix()) - edgeTTSUnixNow()
			edgeTTSClockSkewMutex.Lock()
			edgeTTSClockSkewSeconds += skew
			edgeTTSClockSkewMutex.Unlock()
		}
		if response.Body != nil {
			response.Body.Close()
		}
		response, err = fetch()
		if err != nil {
			return nil
		}
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var voices []struct {
		ShortName    string `json:"ShortName"`
		FriendlyName string `json:"FriendlyName"`
		Locale       string `json:"Locale"`
		Gender       string `json:"Gender"`
	}
	if err := json.NewDecoder(response.Body).Decode(&voices); err != nil {
		return nil
	}
	models := make([]ModelInfo, 0, len(voices))
	for _, voice := range voices {
		if voice.ShortName == "" {
			continue
		}
		label := voice.FriendlyName
		if label == "" {
			label = fmt.Sprintf("%s (%s, %s)", voice.ShortName, voice.Locale, voice.Gender)
		}
		models = append(models, ModelInfo{
			ID: voice.ShortName, Vendor: "microsoft", Label: label,
			Capabilities:      map[string]any{"tts": true, "audio": true},
			SupportedSurfaces: []string{"/v1/audio/speech"},
		})
	}
	return models
}

// Synthesize renders text with the given voice and prosody rate (e.g. "+0%"),
// returning MP3 audio bytes and the served output format.
func (p EdgeTTSProvider) Synthesize(voice, text, rate string) ([]byte, string, error) {
	voice = strings.TrimSpace(voice)
	if voice == "" {
		voice = p.voice
	}
	if strings.TrimSpace(rate) == "" {
		rate = "+0%"
	}
	cleaned := edgeTTSSanitize(text)
	if strings.TrimSpace(cleaned) == "" {
		return nil, "", &InvocationError{Msg: "edge_tts: input text is empty"}
	}
	var audio []byte
	for _, chunk := range edgeTTSSplit(edgeTTSEscapeXML(cleaned), edgeTTSMaxMessageSize) {
		part, err := p.synthesizeChunk(voice, chunk, rate)
		if err != nil {
			return nil, "", err
		}
		audio = append(audio, part...)
	}
	if len(audio) == 0 {
		return nil, "", &InvocationError{Msg: "edge_tts: the service returned no audio"}
	}
	return audio, edgeTTSOutputFormat, nil
}

func (p EdgeTTSProvider) synthesizeChunk(voice, escapedText, rate string) ([]byte, error) {
	connection, err := p.dial()
	if err != nil {
		return nil, err
	}
	defer connection.Close()

	timestamp := time.Now().UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	speechConfig := "X-Timestamp:" + timestamp + "\r\n" +
		"Content-Type:application/json; charset=utf-8\r\n" +
		"Path:speech.config\r\n\r\n" +
		`{"context":{"synthesis":{"audio":{"metadataoptions":{` +
		`"sentenceBoundaryEnabled":"false","wordBoundaryEnabled":"true"},` +
		`"outputFormat":"` + edgeTTSOutputFormat + `"}}}}`
	if err := connection.WriteMessage(websocket.TextMessage, []byte(speechConfig)); err != nil {
		return nil, &InvocationError{Msg: "edge_tts: speech.config write failed: " + err.Error()}
	}

	ssml := "<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='en-US'>" +
		"<voice name='" + voice + "'><prosody pitch='+0Hz' rate='" + rate + "' volume='+0%'>" +
		escapedText + "</prosody></voice></speak>"
	// The trailing Z on X-Timestamp reproduces the service's expected format.
	ssmlMessage := "X-RequestId:" + edgeTTSConnectionID() + "\r\n" +
		"Content-Type:application/ssml+xml\r\n" +
		"X-Timestamp:" + timestamp + "Z\r\n" +
		"Path:ssml\r\n\r\n" + ssml
	if err := connection.WriteMessage(websocket.TextMessage, []byte(ssmlMessage)); err != nil {
		return nil, &InvocationError{Msg: "edge_tts: ssml write failed: " + err.Error()}
	}

	var audio []byte
	deadline := time.Now().Add(p.timeout)
	for {
		if err := connection.SetReadDeadline(deadline); err != nil {
			return nil, &InvocationError{Msg: "edge_tts: " + err.Error()}
		}
		messageType, data, err := connection.ReadMessage()
		if err != nil {
			return nil, &InvocationError{Msg: "edge_tts: read failed: " + err.Error()}
		}
		switch messageType {
		case websocket.TextMessage:
			headers := edgeTTSHeaders(data)
			if headers["Path"] == "turn.end" {
				return audio, nil
			}
		case websocket.BinaryMessage:
			if len(data) < 2 {
				continue
			}
			headerLength := int(binary.BigEndian.Uint16(data[:2]))
			if headerLength+2 > len(data) {
				continue
			}
			headers := edgeTTSHeaders(data[2 : 2+headerLength])
			if headers["Path"] == "audio" {
				audio = append(audio, data[2+headerLength:]...)
			}
		}
	}
}

func (p EdgeTTSProvider) dial() (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		Proxy:             http.ProxyFromEnvironment,
		HandshakeTimeout:  p.timeout,
		EnableCompression: true,
		Subprotocols:      []string{"synthesize"},
	}
	header := http.Header{}
	header.Set("User-Agent", edgeTTSUserAgent)
	header.Set("Origin", "chrome-extension://jdiccldimpdaibmpdkjnbmckianbfold")
	header.Set("Pragma", "no-cache")
	header.Set("Cache-Control", "no-cache")

	dialWebsocket := p.dialWebsocket
	if dialWebsocket == nil {
		dialWebsocket = dialer.Dial
	}
	connection, response, err := dialWebsocket(p.websocketURL(), header)
	if err != nil && response != nil && response.StatusCode == http.StatusForbidden {
		// A 403 usually means the request signature drifted from the service
		// clock. Learn the skew from the server's Date header and retry once.
		if serverDate, parseErr := time.Parse(time.RFC1123, response.Header.Get("Date")); parseErr == nil {
			skew := float64(serverDate.UTC().Unix()) - edgeTTSUnixNow()
			edgeTTSClockSkewMutex.Lock()
			edgeTTSClockSkewSeconds += skew
			edgeTTSClockSkewMutex.Unlock()
		}
		if response.Body != nil {
			response.Body.Close()
		}
		connection, response, err = dialWebsocket(p.websocketURL(), header)
	}
	if err != nil {
		status := 0
		if response != nil {
			status = response.StatusCode
			if response.Body != nil {
				response.Body.Close()
			}
		}
		if status != 0 {
			return nil, invocationStatus(fmt.Sprintf("edge_tts: websocket handshake failed (status %d)", status), status)
		}
		return nil, invocation("edge_tts: websocket transport failed")
	}
	return connection, nil
}

func (p EdgeTTSProvider) websocketURL() string {
	return p.websocketScheme() + p.base + "/websocket/v1?Ocp-Apim-Subscription-Key=" + p.token +
		"&Sec-MS-GEC=" + p.securityToken() +
		"&Sec-MS-GEC-Version=1-" + edgeTTSChromiumFull +
		"&ConnectionId=" + edgeTTSConnectionID()
}

// securityToken derives the Sec-MS-GEC request signature: the SHA-256 of the
// current Windows file time (rounded down to five minutes, in 100ns ticks)
// concatenated with the access token, uppercased.
func (p EdgeTTSProvider) securityToken() string {
	ticks := edgeTTSUnixNow() + edgeTTSWindowsEpoch
	ticks = math.Floor(ticks/300) * 300
	ticks *= 1e7
	digest := sha256.Sum256([]byte(fmt.Sprintf("%.0f%s", ticks, p.token)))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func edgeTTSUnixNow() float64 {
	edgeTTSClockSkewMutex.RLock()
	defer edgeTTSClockSkewMutex.RUnlock()
	return float64(time.Now().UTC().Unix()) + edgeTTSClockSkewSeconds
}

func edgeTTSConnectionID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func edgeTTSHeaders(data []byte) map[string]string {
	headers := map[string]string{}
	text := string(data)
	if separator := strings.Index(text, "\r\n\r\n"); separator >= 0 {
		text = text[:separator]
	}
	for _, line := range strings.Split(text, "\r\n") {
		if key, value, found := strings.Cut(line, ":"); found {
			headers[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return headers
}

// edgeTTSSanitize replaces control characters the service rejects.
func edgeTTSSanitize(text string) string {
	var builder strings.Builder
	for _, r := range text {
		code := int(r)
		if (code >= 0 && code <= 8) || code == 11 || code == 12 || (code >= 14 && code <= 31) {
			builder.WriteRune(' ')
		} else {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func edgeTTSEscapeXML(text string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(text)
}

// edgeTTSSplit breaks escaped text into service-sized chunks on whitespace,
// never splitting inside an XML entity.
func edgeTTSSplit(text string, limit int) []string {
	if len(text) <= limit {
		return []string{text}
	}
	var chunks []string
	remaining := text
	for len(remaining) > limit {
		cut := strings.LastIndexAny(remaining[:limit], " \t\n")
		if cut <= 0 {
			cut = limit
			// Do not split an &...; entity across chunks.
			if ampersand := strings.LastIndex(remaining[:cut], "&"); ampersand >= 0 && !strings.Contains(remaining[ampersand:cut], ";") {
				cut = ampersand
			}
			if cut == 0 {
				cut = limit
			}
		}
		chunks = append(chunks, remaining[:cut])
		remaining = strings.TrimLeft(remaining[cut:], " \t\n")
	}
	if remaining != "" {
		chunks = append(chunks, remaining)
	}
	return chunks
}
