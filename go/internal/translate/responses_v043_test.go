package translate

import "testing"

func TestV043SyntheticResponseIsSchemaComplete(t *testing.T) {
	response := ChatResponseToResponsesWithRequest("model", map[string]any{
		"id": "chatcmpl-test",
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": "ok",
			},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3,
		},
	}, map[string]any{
		"max_output_tokens": 8,
		"tools":             []any{},
		"tool_choice":       "auto",
	})
	for _, required := range []string{
		"id", "object", "created_at", "status", "background", "error",
		"incomplete_details", "instructions", "max_output_tokens",
		"max_tool_calls", "model", "output", "parallel_tool_calls",
		"previous_response_id", "reasoning", "store", "temperature",
		"text", "tool_choice", "tools", "top_logprobs", "top_p",
		"truncation", "usage", "user", "metadata",
	} {
		if _, ok := response[required]; !ok {
			t.Fatalf("response missing %q: %+v", required, response)
		}
	}
	usage := response["usage"].(map[string]any)
	if _, ok := usage["input_tokens_details"].(map[string]any); !ok {
		t.Fatalf("input token details=%+v", usage)
	}
	if _, ok := usage["output_tokens_details"].(map[string]any); !ok {
		t.Fatalf("output token details=%+v", usage)
	}
}

func TestV043ResponsesFallbackPreservesStructuredHistory(t *testing.T) {
	messages, kw, err := ResponsesRequestToChat(map[string]any{
		"input": []any{
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": "look"},
					map[string]any{
						"type": "input_image", "image_url": "data:image/png;base64,AA==",
						"detail": "low",
					},
				},
			},
			map[string]any{
				"type": "function_call", "call_id": "call_1",
				"name": "one", "arguments": `{"a":1}`,
			},
			map[string]any{
				"type": "function_call", "call_id": "call_2",
				"name": "two", "arguments": `{"b":2}`,
			},
			map[string]any{
				"type": "function_call_output", "call_id": "call_1",
				"output": []any{map[string]any{"type": "output_text", "text": "done"}},
			},
		},
		"max_output_tokens": 64,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(messages) != 3 {
		t.Fatalf("messages=%+v", messages)
	}
	calls := messages[1]["tool_calls"].([]any)
	if len(calls) != 2 || kw["_max_output_tokens"] != 64 {
		t.Fatalf("calls=%+v kw=%+v", calls, kw)
	}
	image := messages[0]["content"].([]any)[1].(map[string]any)["image_url"].(map[string]any)
	if image["detail"] != "low" {
		t.Fatalf("image=%+v", image)
	}
}

func TestV043TextOnlyMultipartCollapsesToString(t *testing.T) {
	messages, _, err := ResponsesRequestToChat(map[string]any{
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "hello "},
				map[string]any{"type": "input_text", "text": "world"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if messages[0]["content"] != "hello world" {
		t.Fatalf("content=%+v", messages[0]["content"])
	}
}

func TestV043AnthropicPreservesDeveloperAsSystem(t *testing.T) {
	system, messages := OpenAIMessagesToAnthropic([]map[string]any{
		{"role": "developer", "content": "developer policy"},
		{"role": "user", "content": "hello"},
	})
	if system != "developer policy" || len(messages) != 1 ||
		messages[0]["role"] != "user" {
		t.Fatalf("system=%q messages=%+v", system, messages)
	}
}

func TestV043ResponsesFallbackRejectsLossyFeatures(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"structured output": {
			"input": "hello", "text": map[string]any{"format": map[string]any{"type": "json_schema"}},
		},
		"reasoning summary": {
			"input": "hello", "reasoning": map[string]any{"summary": "auto"},
		},
		"structured instructions": {
			"input": "hello",
			"instructions": []any{map[string]any{
				"type": "input_text", "text": "structured",
			}},
		},
		"file image": {
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{
					"type": "input_image", "file_id": "file_1",
				}},
			}},
		},
		"function image output": {
			"input": []any{map[string]any{
				"type": "function_call_output", "call_id": "call_1",
				"output": []any{map[string]any{
					"type": "input_image", "image_url": "data:image/png;base64,AA==",
				}},
			}},
		},
		"constrained function tool": {
			"input": "hello",
			"tools": []any{map[string]any{
				"type": "function", "name": "restricted",
				"parameters":      map[string]any{"type": "object"},
				"allowed_callers": []any{"code_interpreter"},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ResponsesRequestToChat(payload); err == nil {
				t.Fatal("lossy request unexpectedly converted")
			}
		})
	}
}

func TestV043ChatRefusalAndIncompleteStatusArePreserved(t *testing.T) {
	response := ChatResponseToResponses("model", map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{
				"role": "assistant", "content": nil, "refusal": "cannot comply",
			},
			"finish_reason": "content_filter",
		}},
	})
	if response["status"] != "incomplete" {
		t.Fatalf("response=%+v", response)
	}
	output := response["output"].([]any)
	content := output[0].(map[string]any)["content"].([]any)
	if content[0].(map[string]any)["type"] != "refusal" {
		t.Fatalf("content=%+v", content)
	}
}
