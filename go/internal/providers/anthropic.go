package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"llmgw/internal/translate"
)

const (
	anthropicVersion     = "2023-06-01"
	anthropicDefaultBase = "https://api.anthropic.com"
	anthropicDefaultMax  = 4096
)

// AnthropicNativeProvider forwards to the real Anthropic Messages API. It takes
// OpenAI-shaped messages, translates to Anthropic on the way out, and back.
type AnthropicNativeProvider struct {
	BaseURL string
	APIKey  string
	Timeout float64
}

func (AnthropicNativeProvider) IsStub() bool { return false }

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

func (p AnthropicNativeProvider) payload(model string, messages []Message, stream bool, kw Kwargs) map[string]any {
	system, anthropicMessages := translate.OpenAIMessagesToAnthropic(messages)
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
	if thinking, ok := kw["thinking"].(map[string]any); ok && len(thinking) > 0 {
		payload["thinking"] = thinking
	}
	if outputConfig, ok := kw["output_config"].(map[string]any); ok && len(outputConfig) > 0 {
		payload["output_config"] = outputConfig
	}
	return payload
}

func (p AnthropicNativeProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	body, _ := json.Marshal(p.payload(model, messages, false, kw))
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	req.Header = p.headers()
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, invocation("anthropic: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, invocationStatus(
			fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)),
			resp.StatusCode,
		)
	}
	var anthropicResp map[string]any
	if json.Unmarshal(raw, &anthropicResp) != nil {
		return nil, invocation("anthropic: invalid JSON in upstream response")
	}
	return translate.AnthropicResponseToOpenAI(anthropicResp, model), nil
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
	body, err := json.Marshal(request)
	if err != nil {
		return nil, &ConfigError{Msg: "anthropic: invalid Messages request"}
	}
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	req.Header = p.headers()
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, invocation("anthropic: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, invocationStatus(fmt.Sprintf("anthropic: upstream returned %d: %s", resp.StatusCode, extractError(raw)), resp.StatusCode)
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return nil, invocation("anthropic: invalid JSON in upstream response")
	}
	return result, nil
}

func (p AnthropicNativeProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	body, _ := json.Marshal(p.payload(model, messages, true, kw))
	req, _ := http.NewRequest("POST", p.base()+"/v1/messages", bytes.NewReader(body))
	req.Header = p.headers()
	resp, err := httpClient(p.timeout()).Do(req)
	if err != nil {
		return nil, invocation("anthropic: streaming transport error: " + err.Error())
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
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
	timeout := p.timeout()
	if timeout > 10 {
		timeout = 10
	}
	req, _ := http.NewRequest("GET", p.base()+"/v1/models", nil)
	req.Header = p.headers()
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil
	}
	body, err := decodeJSON(resp.Body)
	if err != nil {
		return nil
	}
	items, ok := body["data"].([]any)
	if !ok {
		return nil
	}
	var out []ModelInfo
	for _, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		label, _ := m["display_name"].(string)
		out = append(out, ModelInfo{
			ID: id, Vendor: "anthropic", Label: label,
			SupportedSurfaces: []string{"/v1/messages"},
		})
	}
	return out
}

// anthropicStreamIter reads the Anthropic SSE body and translates it to OpenAI
// chunk JSON strings lazily via a channel fed by a goroutine.
type anthropicStreamIter struct {
	resp *http.Response
	ch   chan string
	done bool
}

func newAnthropicStreamIter(resp *http.Response, model string) *anthropicStreamIter {
	it := &anthropicStreamIter{resp: resp, ch: make(chan string, 16)}
	go func() {
		defer close(it.ch)
		defer resp.Body.Close()
		reader := newLineReader(resp.Body)
		translate.AnthropicSSEToOpenAIChunks(reader, model, func(chunk string) {
			it.ch <- chunk
		})
	}()
	return it
}

func (it *anthropicStreamIter) Next() (string, bool) {
	c, ok := <-it.ch
	return c, ok
}
func (it *anthropicStreamIter) Err() error   { return nil }
func (it *anthropicStreamIter) Close() error { return nil }
