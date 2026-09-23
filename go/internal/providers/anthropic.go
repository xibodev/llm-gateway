package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/iam"

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

// AnthropicNativeProvider forwards to the real Anthropic Messages API. It takes
// OpenAI-shaped messages, translates to Anthropic on the way out, and back.
type AnthropicNativeProvider struct {
	BaseURL string
	APIKey  string
	Auth    anthropicauth.HeaderSource
	Timeout float64
	Now     func() time.Time
}

func (AnthropicNativeProvider) IsStub() bool { return false }

func (AnthropicNativeProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceMessages
}

func (p AnthropicNativeProvider) base() string {
	if p.BaseURL != "" {
		return p.BaseURL
	}
	return anthropicDefaultBase
}

func (p AnthropicNativeProvider) headers() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", anthropicVersion)
	if p.APIKey != "" {
		h.Set("x-api-key", p.APIKey)
	}
	return h
}

func (p AnthropicNativeProvider) applyHeaders(req *http.Request) error {
	req.Header = p.headers()
	if p.Auth.Kind() != "" {
		return p.Auth.Apply(context.Background(), req)
	}
	return nil
}

func (p AnthropicNativeProvider) payload(model string, messages []Message, stream bool, kw Kwargs) (map[string]any, error) {
	conversion := translate.OpenAIMessagesToAnthropicWithReport(messages)
	if err := conversion.RejectMaterialLoss(); err != nil {
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
	if system != "" {
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

func (p AnthropicNativeProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	payload, err := p.payload(model, messages, false, kw)
	if err != nil {
		return nil, err
	}
	if p.Auth.Kind() == anthropicauth.CredentialSetupToken {
		payload["stream"] = true
		anthropicResp, err := p.completeStreamingPayload(payload)
		if err != nil {
			return nil, err
		}
		converted := translate.AnthropicResponseToOpenAIWithReport(anthropicResp, model)
		if err := converted.RejectMaterialLoss(); err != nil {
			return nil, &ConfigError{Msg: err.Error()}
		}
		return converted.Value, nil
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	if err := p.applyHeaders(req); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, retryableInvocation("anthropic: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := readInvocationResponseBody(resp, "anthropic")
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 400 {
		return nil, invocationStatus(
			fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)),
			resp.StatusCode,
		)
	}
	var anthropicResp map[string]any
	if json.Unmarshal(raw, &anthropicResp) != nil || len(anthropicResp) == 0 {
		return nil, circuitFailureInvocation("anthropic: invalid JSON in upstream response")
	}
	if _, ok := anthropicResp["content"].([]any); !ok {
		return nil, circuitFailureInvocation("anthropic: invalid Messages response payload")
	}
	converted := translate.AnthropicResponseToOpenAIWithReport(anthropicResp, model)
	if err := converted.RejectMaterialLoss(); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	return converted.Value, nil
}

func (p AnthropicNativeProvider) CompleteAnthropicMessages(model string, payload map[string]any) (map[string]any, error) {
	request := make(map[string]any, len(payload))
	for key, value := range payload {
		request[key] = value
	}
	request["model"] = model
	request["stream"] = false
	preamble, _ := request["_llmgw_preamble"].(string)
	delete(request, "_llmgw_preamble")
	if preamble != "" {
		switch system := request["system"].(type) {
		case string:
			request["system"] = preamble + "\n\n" + system
		case []any:
			request["system"] = append([]any{map[string]any{"type": "text", "text": preamble}}, system...)
		default:
			request["system"] = preamble
		}
	}
	if p.Auth.Kind() == anthropicauth.CredentialSetupToken {
		request["stream"] = true
		return p.completeStreamingPayload(request)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, &ConfigError{Msg: "anthropic: invalid Messages request"}
	}
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	if err := p.applyHeaders(req); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, retryableInvocation("anthropic: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := readInvocationResponseBody(resp, "anthropic")
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 400 {
		return nil, invocationStatus(fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)), resp.StatusCode)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || len(result) == 0 {
		return nil, circuitFailureInvocation("anthropic: invalid JSON in upstream response")
	}
	if _, ok := result["content"].([]any); !ok {
		return nil, circuitFailureInvocation("anthropic: invalid Messages response payload")
	}
	return result, nil
}

func (p AnthropicNativeProvider) completeStreamingPayload(payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &ConfigError{Msg: "anthropic: invalid Messages request"}
	}
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	if err := p.applyHeaders(req); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, retryableInvocation("anthropic: streaming transport error: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := readInvocationResponseBody(resp, "anthropic")
		return nil, invocationStatus(fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)), resp.StatusCode)
	}
	reader := newSSERecordReader(resp.Body)
	result := map[string]any{"content": []any{}}
	blocks := map[int]map[string]any{}
	partials := map[int]*strings.Builder{}
	started := false
	stopped := false
	for payload, ok := reader.Next(); ok; payload, ok = reader.Next() {
		if stopped {
			return nil, circuitFailureInvocation("anthropic: data followed streamed Messages terminal event")
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			return nil, circuitFailureInvocation("anthropic: invalid JSON in streamed Messages response")
		}
		switch event["type"] {
		case "message_start":
			if started {
				return nil, circuitFailureInvocation("anthropic: duplicate streamed Messages start event")
			}
			if message, ok := event["message"].(map[string]any); ok {
				started = true
				for key, value := range message {
					result[key] = value
				}
				result["content"] = []any{}
			}
		case "content_block_start":
			index := intOf(event["index"])
			if block, ok := event["content_block"].(map[string]any); ok {
				copy := make(map[string]any, len(block))
				for key, value := range block {
					copy[key] = value
				}
				blocks[index] = copy
				if copy["type"] == "tool_use" {
					partials[index] = &strings.Builder{}
				}
			}
		case "content_block_delta":
			index := intOf(event["index"])
			block := blocks[index]
			delta, _ := event["delta"].(map[string]any)
			if block == nil || delta == nil {
				continue
			}
			switch delta["type"] {
			case "text_delta":
				block["text"] = stringOf(block["text"]) + stringOf(delta["text"])
			case "thinking_delta":
				block["thinking"] = stringOf(block["thinking"]) + stringOf(delta["thinking"])
			case "signature_delta":
				block["signature"] = stringOf(block["signature"]) + stringOf(delta["signature"])
			case "input_json_delta":
				if partials[index] != nil {
					partials[index].WriteString(stringOf(delta["partial_json"]))
				}
			}
		case "content_block_stop":
			index := intOf(event["index"])
			if block := blocks[index]; block != nil {
				if partial := partials[index]; partial != nil {
					var input any = map[string]any{}
					if partial.Len() > 0 && json.Unmarshal([]byte(partial.String()), &input) != nil {
						return nil, circuitFailureInvocation("anthropic: invalid streamed tool input")
					}
					block["input"] = input
				}
				result["content"] = append(result["content"].([]any), block)
				delete(blocks, index)
			}
		case "message_delta":
			if delta, ok := event["delta"].(map[string]any); ok {
				for key, value := range delta {
					result[key] = value
				}
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				current, _ := result["usage"].(map[string]any)
				if current == nil {
					current = map[string]any{}
				}
				for key, value := range usage {
					current[key] = value
				}
				result["usage"] = current
			}
		case "error":
			return nil, circuitFailureInvocation("anthropic: streamed Messages response reported an error")
		case "message_stop":
			if !started || len(blocks) != 0 {
				return nil, circuitFailureInvocation("anthropic: incomplete streamed Messages response")
			}
			stopped = true
		}
	}
	if err := reader.Err(); err != nil {
		return nil, retryableInvocation("anthropic: streaming response error: " + err.Error())
	}
	if !started || !stopped || len(blocks) != 0 {
		return nil, circuitFailureInvocation("anthropic: incomplete streamed Messages response")
	}
	if _, ok := result["content"].([]any); !ok {
		return nil, circuitFailureInvocation("anthropic: invalid streamed Messages response")
	}
	return result, nil
}

func stringOf(value any) string { text, _ := value.(string); return text }

func (p AnthropicNativeProvider) CountAnthropicTokens(model string, payload map[string]any, version string, beta []string) (json.Number, error) {
	request := make(map[string]any, len(payload))
	for key, value := range payload {
		request[key] = value
	}
	request["model"] = model
	body, err := json.Marshal(request)
	if err != nil {
		return "", &ConfigError{Msg: "anthropic: invalid token-count request"}
	}
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err := p.applyHeaders(req); err != nil {
		return "", &ConfigError{Msg: err.Error()}
	}
	if validAnthropicHeader(version) {
		req.Header.Set("anthropic-version", strings.TrimSpace(version))
	}
	for _, value := range beta {
		if validAnthropicHeader(value) {
			req.Header.Add("anthropic-beta", strings.TrimSpace(value))
		}
	}
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return "", retryableInvocation("anthropic: token-count transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := readInvocationResponseBody(resp, "anthropic: token count")
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode >= 400 {
		return "", invocationStatus(fmt.Sprintf("anthropic: token count returned %d: %s", resp.StatusCode, extractError(raw)), resp.StatusCode)
	}
	var result struct {
		InputTokens json.Number `json:"input_tokens"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || !nonnegativeInteger(result.InputTokens.String()) {
		return "", fmt.Errorf("%w: %w", ErrInvalidAnthropicTokenCount, circuitFailureInvocation("anthropic: invalid token-count response"))
	}
	return result.InputTokens, nil
}

func validAnthropicHeader(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func nonnegativeInteger(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (p AnthropicNativeProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload, err := p.payload(model, messages, true, kw)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	if err := p.applyHeaders(req); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, retryableInvocation("anthropic: streaming transport error: " + err.Error())
	}
	if resp.StatusCode >= 400 {
		raw, _ := readInvocationResponseBody(resp, "anthropic")
		resp.Body.Close()
		return nil, invocationStatus(
			fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)),
			resp.StatusCode,
		)
	}
	// Translate Anthropic SSE -> OpenAI chunks eagerly into a buffered iterator.
	return newAnthropicStreamIter(resp, model), nil
}

func (p AnthropicNativeProvider) timeout() float64 {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 60.0
}

func (p AnthropicNativeProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p AnthropicNativeProvider) ListModelsWithError() ([]ModelInfo, *iam.ProviderAccountObservation, error) {
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

// anthropicStreamIter reads the Anthropic SSE body and translates it to OpenAI
// chunk JSON strings lazily via a channel fed by a goroutine.
type anthropicStreamIter struct {
	resp *http.Response
	ch   chan string
	done bool
	err  error
}

func newAnthropicStreamIter(resp *http.Response, model string) *anthropicStreamIter {
	it := &anthropicStreamIter{resp: resp, ch: make(chan string, 16)}
	go func() {
		defer close(it.ch)
		defer resp.Body.Close()
		reader := newSSERecordReader(resp.Body)
		translate.AnthropicSSEToOpenAIChunks(func() (string, bool) {
			payload, ok := reader.Next()
			if !ok {
				return "", false
			}
			return "data: " + payload, true
		}, model, func(chunk string) {
			it.ch <- chunk
		})
		it.err = reader.Err()
	}()
	return it
}

func (it *anthropicStreamIter) Next() (string, bool) {
	c, ok := <-it.ch
	return c, ok
}
func (it *anthropicStreamIter) Err() error   { return it.err }
func (it *anthropicStreamIter) Close() error { return nil }
