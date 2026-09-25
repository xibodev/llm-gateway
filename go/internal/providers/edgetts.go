package providers

// The gateway's provider type "edge_tts": the speech-synthesis service
// behind Microsoft Edge's read-aloud feature, spoken by llmgw-core's Edge
// TTS vertical over the websocket edgetts_websocket.go opens. Core keeps the
// protocol and its baked-in defaults — base host, access token, default
// voice — each of which provider configuration still overrides when the
// upstream service changes, without rebuilding the gateway.

import (
	"context"
	"errors"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// edgeTTSDefaultTimeout is core's default bound on each handshake, and on
// each chunk's exchange after it. The gateway applies it itself because the
// dialer bounds the handshake too, and must bound it alike.
const edgeTTSDefaultTimeout = 60 * time.Second

// SpeechSynthesizer is implemented by providers that produce audio from text.
// The audio speech endpoint and the verify operation prefer it over Complete.
type SpeechSynthesizer interface {
	Synthesize(voice, text, rate string) ([]byte, string, error)
	DefaultVoice() string
}

type ContextSpeechSynthesizer interface {
	SynthesizeContext(context.Context, string, string, string) ([]byte, string, error)
}

// EdgeTTSProvider synthesizes speech through core's Edge TTS. It holds one
// core instance, and the provider cache keeps the provider, so the clock
// skew the instance learns from a refused handshake signs every later
// request of the provider, not only the retry.
type EdgeTTSProvider struct {
	speech *coreproviders.EdgeTTS
	// token is the access token the factory resolved; empty sends the
	// public one.
	token string
}

// NewEdgeTTS builds the provider at baseURL, or Microsoft's service, with
// the access token and default voice given, or core's defaults. An explicit
// http:// or ws:// base opts into plaintext transport (local relays and
// tests); everything else uses TLS. A base core cannot read, or a default
// voice the SSML cannot carry, is a configuration error.
func NewEdgeTTS(baseURL, token, defaultVoice string, timeoutSeconds float64) (*EdgeTTSProvider, error) {
	timeout := time.Duration(timeoutSeconds * float64(time.Second))
	if timeout <= 0 {
		timeout = edgeTTSDefaultTimeout
	}
	return newEdgeTTS(coreproviders.EdgeTTSConfig{
		Dial: edgeTTSDialer(timeout), BaseURL: baseURL, DefaultVoice: defaultVoice, Timeout: timeout,
	}, token)
}

// newEdgeTTS builds the provider over core's Edge TTS as config configures
// it; tests supply the dialer and the clock.
func newEdgeTTS(config coreproviders.EdgeTTSConfig, token string) (*EdgeTTSProvider, error) {
	speech, err := coreproviders.NewEdgeTTS(config)
	if err != nil {
		return nil, &ConfigError{Msg: "edge_tts: " + err.Error()}
	}
	return &EdgeTTSProvider{speech: speech, token: strings.TrimSpace(token)}, nil
}

func (p *EdgeTTSProvider) DefaultVoice() string { return p.speech.DefaultVoice() }
func (p *EdgeTTSProvider) IsStub() bool         { return false }

// Complete exists to satisfy the Provider interface; edge_tts has no chat API.
func (p *EdgeTTSProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return nil, &ConfigError{Msg: "edge_tts synthesizes speech only — call POST /v1/audio/speech with this provider's voice as the model"}
}

func (p *EdgeTTSProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: "edge_tts synthesizes speech only — call POST /v1/audio/speech with this provider's voice as the model"}
}

// credential is the access token as core reads one: an API key, or none
// for the public token.
func (p *EdgeTTSProvider) credential() *core.Credential {
	if p.token == "" {
		return nil
	}
	return &core.Credential{APIKey: p.token}
}

// ListModels exposes the service's voices as catalog models so voices appear
// in /v1/models, route building, and the provider detail page. A voice list
// that cannot be read lists nothing, which the catalog reports as
// unavailable.
func (p *EdgeTTSProvider) ListModels() []ModelInfo {
	voices, err := p.speech.ListModels(context.Background(), p.credential())
	if err != nil {
		return nil
	}
	models := make([]ModelInfo, 0, len(voices))
	for _, voice := range voices {
		models = append(models, ModelInfo{
			ID: voice.ID, Vendor: voice.Vendor, Label: voice.DisplayName,
			Capabilities: voice.LegacyCapabilities, SupportedSurfaces: voice.SupportedAPIs,
		})
	}
	return models
}

// Synthesize renders text with the given voice and prosody rate (e.g. "+0%"),
// returning MP3 audio bytes and the served output format.
func (p *EdgeTTSProvider) Synthesize(voice, text, rate string) ([]byte, string, error) {
	return p.SynthesizeContext(context.Background(), voice, text, rate)
}

func (p *EdgeTTSProvider) SynthesizeContext(ctx context.Context, voice, text, rate string) ([]byte, string, error) {
	audio, err := p.speech.Synthesize(ctx, p.credential(), voice, text, rate)
	if err != nil {
		return nil, "", edgeTTSFailure(ctx, err)
	}
	return audio, coreproviders.EdgeTTSOutputFormat, nil
}

// edgeTTSFailure returns the gateway error for what core's Edge TTS returned,
// so the audio endpoint and the resilience wrapper decide on it as they did
// on the gateway's own client. A refused handshake keeps its status, and no
// Retry-After, which that client never read; a websocket that broke may
// repeat; audio that cannot be used counts against the circuit; and a
// request core would not send, such as text with nothing to speak, fails
// the invocation, as empty text did. An access token the URLs cannot carry
// is a configuration error. Core's messages quote no URL, which carries the
// access token and the signature.
func edgeTTSFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		return invocation("edge_tts: synthesis failed")
	}
	message := "edge_tts: " + failure.Message
	switch classification := failure.Classification; {
	case failure.Class == core.ProviderErrorConfiguration:
		return &ConfigError{Msg: message}
	case classification.StatusCode != 0:
		return invocationStatus(message, classification.StatusCode)
	case classification.Retryable:
		return retryableInvocation(message)
	case classification.CircuitFailure:
		return circuitFailureInvocation(message)
	}
	return invocation(message)
}
