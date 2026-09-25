package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
)

const (
	anthropicVersion     = "2023-06-01"
	anthropicDefaultBase = "https://api.anthropic.com"
	anthropicDefaultMax  = 4096
	anthropicCatalogTTL  = time.Hour
)

// AnthropicNativeProvider is the gateway's Anthropic facade. Messages, Chat
// and the Chat stream go through the core Runtime, which resolves the
// caller's credential through Anthropic's store on each request, and core's
// Anthropic, which sends each Messages request and reads each answer as the
// gateway's transport did, a setup token's completion assembled from its
// stream included. Token counts go through core.CountTokens.
//
// What the gateway did above that transport stays here. Core has no route
// from Chat to Messages, so a Chat request is converted to Messages with
// llm-translate and its answer converted back, and a Chat stream is
// re-encoded as the chunks the API layer reads, which it renders as the
// events a Messages client reads today.
//
// The catalog stays on the gateway's path, with the exported fields and the
// factory's credential: the gateway never asks the core Runtime for a
// catalog, and nothing the vertical serves reads one. A facade the factory
// did not build serves only the catalog.
type AnthropicNativeProvider struct {
	BaseURL string
	Auth    anthropicauth.HeaderSource
	Timeout float64
	Now     func() time.Time

	runtime  *Runtime
	instance string
	caller   core.Caller
}

func (AnthropicNativeProvider) IsStub() bool { return false }

func (AnthropicNativeProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceMessages
}

// operation returns the context of an operation, naming Anthropic and the
// facade's caller for the store and the provider the core Runtime calls. It
// derives from no caller's context, because the transport took none: a
// request runs until its HTTP client times out, whatever the router's
// deadline.
func (p AnthropicNativeProvider) operation() context.Context {
	return withCoreOperation(context.Background(), anthropicCoreType, p.caller)
}

// anthropicMessagesRequest is the core request of a Messages payload. Core's
// Anthropic decodes it with its numbers kept as written and re-encodes it as
// the transport encoded a body, so the upstream body is unchanged.
func anthropicMessagesRequest(model string, payload map[string]any, invalid string) (core.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, &ConfigError{Msg: invalid}
	}
	return core.Request{Surface: core.ModelSurfaceMessages, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
}

// invoke sends a Messages payload through the core Runtime. A request that
// gets no answer keeps the transport's message, which named how it was sent:
// a setup token's completion is requested as a stream.
func (p AnthropicNativeProvider) invoke(model string, payload map[string]any) (core.Response, error) {
	request, err := anthropicMessagesRequest(model, payload, "anthropic: invalid Messages request")
	if err != nil {
		return core.Response{}, err
	}
	response, err := p.runtime.core.Invoke(p.operation(), p.caller, p.instance, request)
	if err != nil {
		transport := "anthropic: upstream transport error: "
		if p.Auth.Kind() == anthropicauth.CredentialSetupToken {
			transport = "anthropic: streaming transport error: "
		}
		return core.Response{}, anthropicFailure(err, transport, p.instance)
	}
	return response, nil
}

// payload converts a Chat request to the Messages payload it is sent as.
func (p AnthropicNativeProvider) payload(model string, messages []Message, stream bool, kw Kwargs) (map[string]any, error) {
	conversion := translate.OpenAIMessagesToAnthropicWithReport(messages)
	if err := RejectMaterialLossExceptThoughtSignatures(conversion.Report); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	system, anthropicMessages := conversion.Value.System, conversion.Value.Messages
	maxTokens := anthropicDefaultMax
	if v := intOf(kw["max_tokens"]); v > 0 {
		maxTokens = v
	} else if v := intOf(kw["_max_output_tokens"]); v > 0 {
		maxTokens = v
	}
	payload := map[string]any{
		"model": model, "messages": anthropicMessages, "stream": stream, "max_tokens": maxTokens,
	}
	// A system string cannot hold cache breakpoints. SystemBlocks is set only
	// when a system part carries cache_control, and holds the same text split
	// at each breakpoint, so the prefix the client marked is the one cached.
	if blocks := conversion.Value.SystemBlocks; len(blocks) > 0 {
		payload["system"] = blocks
	} else if system != "" {
		payload["system"] = system
	}
	if v, ok := kw["temperature"]; ok && v != nil {
		payload["temperature"] = v
	}
	if v, ok := kw["top_p"]; ok && v != nil {
		payload["top_p"] = v
	}
	if stop, ok := kw["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case string:
			payload["stop_sequences"] = []any{s}
		case []any:
			payload["stop_sequences"] = s
		case []string:
			arr := make([]any, len(s))
			for i, v := range s {
				arr[i] = v
			}
			payload["stop_sequences"] = arr
		}
	}
	if tools, ok := kw["tools"].([]any); ok {
		if t := translate.OpenAIToolsToAnthropic(tools); t != nil {
			payload["tools"] = t
		}
	}
	if metadata, ok := kw["metadata"].(map[string]any); ok {
		payload["metadata"] = metadata
	}
	if thinking, ok := kw["thinking"].(map[string]any); ok && len(thinking) > 0 {
		payload["thinking"] = thinking
	}
	if outputConfig, ok := kw["output_config"].(map[string]any); ok && len(outputConfig) > 0 {
		payload["output_config"] = outputConfig
	}
	return payload, nil
}

// Complete sends a Chat request as Messages and converts the answer back,
// decoded as the transport decoded the answers it converted.
func (p AnthropicNativeProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	payload, err := p.payload(model, messages, false, kw)
	if err != nil {
		return nil, err
	}
	response, err := p.invoke(model, payload)
	if err != nil {
		return nil, err
	}
	var anthropicResp map[string]any
	if json.Unmarshal(response.Body, &anthropicResp) != nil {
		return nil, circuitFailureInvocation("anthropic: invalid JSON in upstream response")
	}
	converted := translate.AnthropicResponseToOpenAIWithReport(anthropicResp, model)
	if err := converted.RejectMaterialLoss(); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	return converted.Value, nil
}

// CompleteAnthropicMessages passes a Messages payload through. Core's
// Anthropic sets its model and stream flag and merges the _llmgw_preamble the
// API layer set into the system prompt, as the transport did, and the answer
// is decoded with its numbers kept as Anthropic wrote them.
func (p AnthropicNativeProvider) CompleteAnthropicMessages(model string, payload map[string]any) (map[string]any, error) {
	response, err := p.invoke(model, payload)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return nil, circuitFailureInvocation("anthropic: invalid JSON in upstream response")
	}
	return result, nil
}

// CountAnthropicTokens counts through core.CountTokens, which the core
// Runtime does not perform, with the credential Anthropic's store resolves
// for the caller, as a request would be sent. Core's Anthropic forwards the
// anthropic-version and anthropic-beta values the transport forwarded.
func (p AnthropicNativeProvider) CountAnthropicTokens(model string, payload map[string]any, version string, beta []string) (json.Number, error) {
	request, err := anthropicMessagesRequest(model, payload, "anthropic: invalid token-count request")
	if err != nil {
		return "", err
	}
	counter, err := newCoreAnthropic(p.instance, p.BaseURL, p.Timeout)
	if err != nil {
		return "", err
	}
	ctx := p.operation()
	if request.Credential, err = p.runtime.coreCredential(ctx, anthropicCoreType, p.caller, p.instance); err != nil {
		return "", anthropicCredentialFailure(err, p.instance)
	}
	header := http.Header{}
	header.Set("anthropic-version", version)
	for _, value := range beta {
		header.Add("anthropic-beta", value)
	}
	count, err := core.CountTokens(ctx, counter, core.TokenCountRequest{Request: request, Header: header})
	if err != nil {
		return "", anthropicCountFailure(err, p.instance)
	}
	return json.Number(strconv.FormatInt(count.InputTokens, 10)), nil
}

// Stream sends a Chat request as a Messages stream and re-encodes the stream
// core relays as Chat chunks.
func (p AnthropicNativeProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload, err := p.payload(model, messages, true, kw)
	if err != nil {
		return nil, err
	}
	request, err := anthropicMessagesRequest(model, payload, "anthropic: invalid Messages request")
	if err != nil {
		return nil, err
	}
	stream, err := p.runtime.core.Stream(p.operation(), p.caller, p.instance, request)
	if err != nil {
		return nil, anthropicFailure(err, "anthropic: streaming transport error: ", p.instance)
	}
	return newAnthropicChatStream(stream, model), nil
}

// anthropicChatStream re-encodes core's Anthropic stream as Chat chunks, as
// the transport's stream did: llm-translate reads the data of each record
// core relays and writes the chunks, which a goroutine hands to Next. It ends
// as that stream ended; see anthropicStreamEnd.
type anthropicChatStream struct {
	inner  core.StreamIter
	chunks chan string
	done   chan struct{}
	closed sync.Once
	err    error
}

func newAnthropicChatStream(inner core.StreamIter, model string) *anthropicChatStream {
	s := &anthropicChatStream{inner: inner, chunks: make(chan string, 16), done: make(chan struct{})}
	go func() {
		defer close(s.chunks)
		translate.AnthropicSSEToOpenAIChunks(s.line, model, s.emit)
	}()
	return s
}

// line is the data line llm-translate reads next, as the transport fed it.
func (s *anthropicChatStream) line() (string, bool) {
	data, err := nextCoreData(s.inner)
	if err != nil {
		s.err = anthropicStreamEnd(err)
		return "", false
	}
	return "data: " + data, true
}

// emit hands a chunk to Next, or drops it once the stream is closed, so the
// goroutine never outlives a reader that stopped reading.
func (s *anthropicChatStream) emit(chunk string) {
	select {
	case s.chunks <- chunk:
	case <-s.done:
	}
}

func (s *anthropicChatStream) Next() (string, bool) {
	chunk, ok := <-s.chunks
	return chunk, ok
}

func (s *anthropicChatStream) Err() error { return s.err }

// Close releases the goroutine and closes core's stream, which ends a read
// in progress.
func (s *anthropicChatStream) Close() error {
	s.closed.Do(func() { close(s.done) })
	return s.inner.Close()
}

func (p AnthropicNativeProvider) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return anthropicDefaultBase
}

// applyHeaders sets a catalog request's headers as the transport set every
// request's: the content type and anthropic-version, then the credential's.
func (p AnthropicNativeProvider) applyHeaders(req *http.Request) error {
	req.Header = http.Header{}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if p.Auth.Kind() != "" {
		return p.Auth.Apply(context.Background(), req)
	}
	return nil
}

func (p AnthropicNativeProvider) timeout() float64 { return anthropicTimeout(p.Timeout) }

func (p AnthropicNativeProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p AnthropicNativeProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	timeout := p.timeout()
	if timeout > 10 {
		timeout = 10
	}
	req, err := http.NewRequest("GET", p.base()+"/v1/models", nil)
	if err != nil {
		return nil, nil, catalogError("catalog_transport_error", "Provider catalog request could not be created.", 0)
	}
	if err := p.applyHeaders(req); err != nil {
		return nil, nil, &ConfigError{Msg: err.Error()}
	}
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil, nil, catalogError("catalog_transport_error", "Provider catalog request could not reach the upstream service.", 0)
	}
	body, err := decodeCatalogResponse(resp, "data", "id")
	if err != nil {
		return nil, nil, err
	}
	items := body["data"].([]any)
	discoveredAt := time.Now().UTC()
	if p.Now != nil {
		discoveredAt = p.Now().UTC()
	}
	out := make([]ModelInfo, 0, len(items))
	for _, entry := range items {
		m := entry.(map[string]any)
		id := m["id"].(string)
		label, _ := m["display_name"].(string)
		out = append(out, ModelInfo{
			ID: id, Vendor: "anthropic", Label: label,
			SupportedSurfaces: []string{"/v1/messages"},
			TypedCapabilities: anthropicModelCapabilities(discoveredAt),
		})
	}
	return out, nil, nil
}

func anthropicModelCapabilities(discoveredAt time.Time) *core.ModelCapabilities {
	expiresAt := discoveredAt.Add(anthropicCatalogTTL)
	return &core.ModelCapabilities{
		SchemaVersion: core.ModelCapabilitiesSchemaVersion,
		Operations:    core.ModelOperationCapabilities{Chat: core.SupportSupported},
		Surfaces: core.ModelSurfaceCapabilities{
			ChatCompletions: core.SupportUnsupported,
			Responses:       core.SupportUnsupported,
			Messages:        core.SupportSupported,
		},
		Streaming: core.SupportSupported,
		Provenance: core.ModelCapabilityProvenance{
			Source: core.ModelCapabilitySourceRegistryStatic, Confidence: core.ModelCapabilityConfidenceHigh,
		},
		Freshness: core.ModelCapabilityFreshness{DiscoveredAt: &discoveredAt, ExpiresAt: &expiresAt},
	}
}
