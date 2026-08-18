package providers

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OllamaProvider talks to a local Ollama daemon over its native /api/chat,
// preserving roles and tool-calling. Keyless.
type OllamaProvider struct {
	BaseURL string
	Timeout float64
}

func (OllamaProvider) IsStub() bool { return false }

func ollamaNormalizeMessages(messages []Message) []map[string]any {
	out := []map[string]any{}
	for _, m := range messages {
		role, _ := m["role"].(string)
		if role == "" {
			role = "user"
		}
		if role == "developer" {
			role = "system"
		}
		content := ""
		if c, ok := m["content"].(string); ok {
			content = c
		} else if m["content"] != nil {
			content = fmt.Sprintf("%v", m["content"])
		}
		toolCalls := m["tool_calls"]
		toolCallID, _ := m["tool_call_id"].(string)
		if strings.TrimSpace(content) == "" && toolCalls == nil && toolCallID == "" {
			continue
		}
		msg := map[string]any{"role": role, "content": content}
		if toolCalls != nil {
			msg["tool_calls"] = toolCalls
		}
		if toolCallID != "" {
			msg["tool_call_id"] = toolCallID
		}
		if name, ok := m["name"].(string); ok && name != "" {
			msg["name"] = name
		}
		out = append(out, msg)
	}
	return out
}

func ollamaBuildPayload(model string, messages []Message, stream bool, kw Kwargs) map[string]any {
	payload := map[string]any{
		"model": model, "messages": ollamaNormalizeMessages(messages), "stream": stream,
	}
	options := map[string]any{}
	if v, ok := kw["temperature"]; ok && v != nil {
		options["temperature"] = v
	}
	if v, ok := kw["max_tokens"]; ok && v != nil {
		options["num_predict"] = v
	} else if v, ok := kw["_max_output_tokens"]; ok && v != nil {
		options["num_predict"] = v
	}
	if v, ok := kw["top_p"]; ok && v != nil {
		options["top_p"] = v
	}
	if len(options) > 0 {
		payload["options"] = options
	}
	if tools, ok := kw["tools"]; ok && tools != nil {
		payload["tools"] = tools
	}
	return payload
}

func ollamaMapToolCalls(raw any) []any {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	var mapped []any
	for i, tc := range list {
		tcm, _ := tc.(map[string]any)
		fn, _ := tcm["function"].(map[string]any)
		if fn == nil {
			fn = map[string]any{}
		}
		args := fn["arguments"]
		var argStr string
		if s, ok := args.(string); ok {
			argStr = s
		} else {
			b, _ := json.Marshal(args)
			argStr = string(b)
		}
		id, _ := tcm["id"].(string)
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		name, _ := fn["name"].(string)
		mapped = append(mapped, map[string]any{
			"index": i, "id": id, "type": "function",
			"function": map[string]any{"name": name, "arguments": argStr},
		})
	}
	return mapped
}

func ollamaUsage(data map[string]any) map[string]any {
	prompt := intOf(data["prompt_eval_count"])
	completion := intOf(data["eval_count"])
	return map[string]any{
		"prompt_tokens": prompt, "completion_tokens": completion,
		"total_tokens": prompt + completion,
	}
}

func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

func (p OllamaProvider) chatURL() string { return strings.TrimRight(p.BaseURL, "/") + "/api/chat" }

func (p OllamaProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	payload := ollamaBuildPayload(model, messages, false, kw)
	body, _ := json.Marshal(payload)
	resp, err := httpClient(p.Timeout).Post(p.chatURL(), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, invocation("ollama: request failed: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, invocation(fmt.Sprintf("ollama: request failed (%d): %s", resp.StatusCode, extractError(raw)))
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil {
		return nil, invocation("ollama: invalid response payload")
	}
	message, ok := data["message"].(map[string]any)
	if !ok {
		return nil, invocation("ollama: invalid response payload")
	}
	content, _ := message["content"].(string)
	toolCalls := ollamaMapToolCalls(message["tool_calls"])
	if content == "" && toolCalls == nil {
		return nil, invocation("ollama: returned an empty response")
	}
	outMsg := map[string]any{"role": "assistant", "content": content}
	finish := "stop"
	if toolCalls != nil {
		outMsg["tool_calls"] = toolCalls
		finish = "tool_calls"
	}
	return map[string]any{
		"id": "chatcmpl-ollama", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": outMsg, "finish_reason": finish}},
		"usage":   ollamaUsage(data),
	}, nil
}

// ollamaStreamIter converts Ollama's native NDJSON stream into OpenAI chunk JSON.
type ollamaStreamIter struct {
	resp    *http.Response
	reader  *bufio.Reader
	model   string
	done    bool
	emitted bool
	err     error
}

func (it *ollamaStreamIter) Next() (string, bool) {
	if it.done {
		return "", false
	}
	for {
		line, err := it.reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(line)), &event) == nil {
				message, _ := event["message"].(map[string]any)
				if message == nil {
					message = map[string]any{}
				}
				deltaContent, _ := message["content"].(string)
				toolCalls := ollamaMapToolCalls(message["tool_calls"])
				isDone, _ := event["done"].(bool)
				if deltaContent != "" || toolCalls != nil {
					delta := map[string]any{}
					if deltaContent != "" {
						delta["content"] = deltaContent
					}
					if toolCalls != nil {
						delta["tool_calls"] = toolCalls
					}
					it.emitted = true
					if isDone {
						it.done = true
					}
					return it.chunk(delta, nil), true
				}
				if isDone {
					it.done = true
					finish := "stop"
					if toolCalls != nil {
						finish = "tool_calls"
					}
					return it.chunk(map[string]any{}, finish), true
				}
			}
		}
		if err != nil {
			it.done = true
			if err != io.EOF {
				it.err = invocation("ollama: streaming transport error: " + err.Error())
			} else if !it.emitted {
				it.err = invocation("ollama: returned an empty response")
			}
			return "", false
		}
	}
}

func (it *ollamaStreamIter) chunk(delta map[string]any, finish any) string {
	b, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-ollama", "object": "chat.completion.chunk", "model": it.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	})
	return string(b)
}

func (it *ollamaStreamIter) Err() error { return it.err }
func (it *ollamaStreamIter) Close() error {
	if it.resp != nil {
		return it.resp.Body.Close()
	}
	return nil
}

func (p OllamaProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload := ollamaBuildPayload(model, messages, true, kw)
	body, _ := json.Marshal(payload)
	resp, err := httpClient(p.Timeout).Post(p.chatURL(), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, invocation("ollama: streaming request failed: " + err.Error())
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, invocation(fmt.Sprintf("ollama: request failed (%d): %s", resp.StatusCode, extractError(raw)))
	}
	return &ollamaStreamIter{resp: resp, reader: bufio.NewReader(resp.Body), model: model}, nil
}

func (p OllamaProvider) ListModels() []ModelInfo {
	timeout := p.Timeout
	if timeout > 10 {
		timeout = 10
	}
	resp, err := httpClient(timeout).Get(strings.TrimRight(p.BaseURL, "/") + "/api/tags")
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
	items, ok := body["models"].([]any)
	if !ok {
		return nil
	}
	var out []ModelInfo
	for _, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			name, _ = m["model"].(string)
		}
		if name == "" {
			continue
		}
		vendor := "ollama"
		if details, ok := m["details"].(map[string]any); ok {
			if fam, ok := details["family"].(string); ok && fam != "" {
				vendor = fam
			}
		}
		out = append(out, ModelInfo{ID: name, Vendor: vendor})
	}
	return out
}
