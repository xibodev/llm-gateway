package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// Each provider type is weighed for the Chat fields its facade sends: an
// OpenAI-compatible instance sends every field unless adaptation serves the
// model over Responses, and every other type the fields it names. A value
// that asks for nothing is never a reason to refuse a target.
func TestUnsentChatFieldFollowsWhatEachFacadeSends(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	old := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = old }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"openai":      {Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1"},
			"adapt":       {Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1", ForceApiSupport: true},
			"bedrock":     {Type: "bedrock", BaseURL: "http://127.0.0.1:9/v1"},
			"azure":       {Type: "azure_openai", BaseURL: "http://127.0.0.1:9"},
			"anthropic":   {Type: "anthropic"},
			"studio":      {Type: "ai_studio"},
			"vertex":      {Type: "vertex_ai", Project: "fixture-project"},
			"ollama":      {Type: "ollama"},
			"copilot":     {Type: "github_copilot"},
			"codex":       {Type: "openai_compatible", RegistryID: "openai_codex"},
			"antigravity": {Type: "google_antigravity"},
			"zen":         {Type: "opencode_zen_anonymous"},
			"setup":       {Type: "anthropic_setup_token"},
			"echo":        {Type: "echo"},
			"disabled":    {Type: "anthropic", Disabled: true},
		}
	})
	runtime.catalogs.store(catalogCacheKey("adapt", gatewayCaller()), []ModelInfo{
		{ID: "responses-model", SupportedSurfaces: []string{"/responses"}},
		{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
	})
	jsonMode := map[string]any{"type": "json_object"}
	tool := []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}}
	asksNothing := Kwargs{
		"n": 1.0, "logprobs": false, "top_logprobs": 0.0, "presence_penalty": 0.0, "frequency_penalty": 0.0,
		"response_format": map[string]any{"type": "text"}, "modalities": []any{"text"}, "tool_choice": "auto",
		"parallel_tool_calls": true, "user": "", "logit_bias": map[string]any{}, "seed": nil,
		"stream_options": map[string]any{"include_usage": true}, "_affinity_key": "fixture",
	}
	for _, tc := range []struct {
		name, provider, model string
		kw                    Kwargs
		want                  string
	}{
		{"OpenAI-compatible sends every field", "openai", "any", Kwargs{"response_format": jsonMode, "seed": 7.0, "top_k": 40.0, "logprobs": true}, ""},
		{"adaptation off for a Chat model", "adapt", "chat-model", Kwargs{"response_format": jsonMode}, ""},
		{"adaptation over Responses", "adapt", "responses-model", Kwargs{"response_format": jsonMode, "temperature": 0.2, "tools": tool}, "response_format"},
		{"adaptation refuses stop", "adapt", "responses-model", Kwargs{"stop": []any{"END"}}, "stop"},
		{"a request that turns adaptation off", "adapt", "responses-model", Kwargs{"response_format": jsonMode, "_force_api_support": false}, ""},
		{"Bedrock sends the transport's fields", "bedrock", "any", Kwargs{"parallel_tool_calls": false, "tool_choice": "required", "reasoning_effort": "high"}, ""},
		{"Bedrock", "bedrock", "any", Kwargs{"seed": 7.0, "logprobs": true}, "logprobs"},
		{"Bedrock drops an advisory field", "bedrock", "any", Kwargs{"seed": 7.0}, ""},
		{"Azure OpenAI sends the fields its v1 Chat defines", "azure", "any", Kwargs{"parallel_tool_calls": false, "metadata": map[string]any{"run": "fixture"}, "response_format": jsonMode, "n": 2.0, "logprobs": true, "verbosity": "low"}, ""},
		{"Azure OpenAI", "azure", "any", Kwargs{"web_search_options": map[string]any{}, "thinking": map[string]any{"type": "enabled"}}, "thinking"},
		{"Anthropic sends what Messages carries", "anthropic", "any", Kwargs{"temperature": 0.2, "top_p": 0.9, "max_tokens": 64.0, "stop": "END", "tools": tool, "metadata": map[string]any{"user_id": "u"}}, ""},
		{"Anthropic response_format", "anthropic", "any", Kwargs{"response_format": jsonMode}, "response_format"},
		{"Anthropic logprobs", "anthropic", "any", Kwargs{"logprobs": true}, "logprobs"},
		{"Anthropic n", "anthropic", "any", Kwargs{"n": 2.0}, "n"},
		{"Anthropic tool_choice", "anthropic", "any", Kwargs{"tools": tool, "tool_choice": "required"}, "tool_choice"},
		{"Anthropic reads max_completion_tokens as max_tokens", "anthropic", "any", Kwargs{"max_completion_tokens": 64.0}, ""},
		{"advisory fields are served", "anthropic", "any", Kwargs{"user": "fixture-user", "seed": 7.0, "service_tier": "auto", "store": true, "prompt_cache_key": "fixture", "safety_identifier": "fixture", "reasoning_effort": "high", "presence_penalty": 0.5, "frequency_penalty": 0.5}, ""},
		{"an empty web_search_options turns search on", "anthropic", "any", Kwargs{"web_search_options": map[string]any{}}, "web_search_options"},
		{"the first field in name order", "anthropic", "any", Kwargs{"seed": 7.0, "logprobs": true}, "logprobs"},
		{"values that ask for nothing", "anthropic", "any", asksNothing, ""},
		{"AI Studio sends max_tokens and temperature", "studio", "any", Kwargs{"max_tokens": 64.0, "temperature": 0.2}, ""},
		{"AI Studio", "studio", "any", Kwargs{"stop": "END"}, "stop"},
		{"AI Studio reads max_completion_tokens and drops top_p", "studio", "any", Kwargs{"max_completion_tokens": 64.0, "top_p": 0.9}, ""},
		{"Vertex AI", "vertex", "any", Kwargs{"tools": tool}, "tools"},
		{"Ollama sends its options and tools", "ollama", "any", Kwargs{"temperature": 0.2, "top_p": 0.9, "max_tokens": 64.0, "tools": tool}, ""},
		{"Ollama", "ollama", "any", Kwargs{"stop": "END"}, "stop"},
		{"Copilot sends the transport's fields", "copilot", "any", Kwargs{"parallel_tool_calls": false, "thinking": map[string]any{"type": "disabled"}}, ""},
		{"Copilot", "copilot", "any", Kwargs{"response_format": jsonMode}, "response_format"},
		{"Zen sends the transport's fields", "zen", "any", Kwargs{"parallel_tool_calls": false, "tool_choice": "required", "thinking": map[string]any{"type": "disabled"}}, ""},
		{"Zen", "zen", "any", Kwargs{"response_format": jsonMode}, "response_format"},
		{"Codex sends tool_choice and parallel_tool_calls", "codex", "any", Kwargs{"reasoning_effort": "high", "tool_choice": "required", "parallel_tool_calls": false, "metadata": map[string]any{"run": "fixture"}}, ""},
		{"Codex", "codex", "any", Kwargs{"response_format": jsonMode}, "response_format"},
		{"Antigravity sends what Chat always handed it", "antigravity", "any", Kwargs{"temperature": 0.2, "max_tokens": 64.0, "tools": tool, "tool_choice": "auto"}, ""},
		{"Antigravity", "antigravity", "any", Kwargs{"tools": tool, "tool_choice": "required"}, "tool_choice"},
		{"another daemon type sends what Chat always handed it", "setup", "any", Kwargs{"reasoning_effort": "high", "tool_choice": "required", "metadata": map[string]any{"run": "fixture"}}, ""},
		{"another daemon type", "setup", "any", Kwargs{"parallel_tool_calls": false}, "parallel_tool_calls"},
		{"the echo stub makes no claim", "echo", "any", Kwargs{"response_format": jsonMode}, ""},
		{"a disabled provider makes no claim", "disabled", "any", Kwargs{"response_format": jsonMode}, ""},
		{"an unknown provider makes no claim", "missing", "any", Kwargs{"response_format": jsonMode}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := config.Get()
			field, unsent := runtime.UnsentChatField(settings.Providers[tc.provider], tc.provider, tc.model, gatewayCaller(), tc.kw)
			if field != tc.want || unsent != (tc.want != "") {
				t.Fatalf("field=%q unsent=%v, want %q", field, unsent, tc.want)
			}
		})
	}
}

// A client's own Chat request reaches an OpenAI-compatible upstream with
// every field it set, a field the gateway does not know included, while a
// request translated from another surface still carries only what the
// OpenAI transport forwards. Bedrock forwards only the latter either way,
// which is why routing refuses it the other fields.
func TestOpenAICompatibleForwardsEveryFieldOfAClientChatRequest(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	messages := []Message{{"role": "user", "content": "hi"}}
	fields := map[string]any{
		"response_format": map[string]any{"type": "json_object"}, "seed": 7.0, "n": 2.0, "logprobs": true,
		"user": "fixture-user", "parallel_tool_calls": false, "top_k": 40.0,
	}
	client := Kwargs{ChatFieldsAsSent: true, "temperature": 0.2, "_affinity_key": "fixture"}
	for name, value := range fields {
		client[name] = value
	}
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, APIKey: "fixture-key"})
	for _, stream := range []bool{false, true} {
		var err error
		if stream {
			iter, streamErr := provider.Stream("chat-model", messages, client)
			_, err = drainZenStream(t, iter, streamErr)
		} else {
			_, err = provider.Complete("chat-model", messages, client)
		}
		calls := upstream.take()
		if err != nil || len(calls) != 1 {
			t.Fatalf("stream=%v: err=%v calls=%+v", stream, err, calls)
		}
		var body map[string]any
		_ = json.Unmarshal([]byte(calls[0].body), &body)
		for name, value := range fields {
			if !reflect.DeepEqual(body[name], value) {
				t.Errorf("stream=%v: %s=%#v, want %#v", stream, name, body[name], value)
			}
		}
		for name := range body {
			if strings.HasPrefix(name, "_") || name == "force_api_support" {
				t.Errorf("stream=%v: the gateway's own %q reached the upstream", stream, name)
			}
		}
	}

	if _, err := provider.Complete("chat-model", messages, Kwargs{"output_config": map[string]any{"effort": "low"}, "seed": 7.0}); err != nil {
		t.Fatal(err)
	}
	if calls := upstream.take(); len(calls) != 1 || strings.Contains(calls[0].body, "output_config") || strings.Contains(calls[0].body, "seed") {
		t.Fatalf("a translated request: calls=%+v", calls)
	}

	bedrock := openAICompatibleFixture(t, &config.ProviderConfig{Type: "bedrock", BaseURL: base, APIKey: "fixture-key"})
	if _, err := bedrock.Complete("chat-model", messages, client); err != nil {
		t.Fatal(err)
	}
	if calls := upstream.take(); len(calls) != 1 || strings.Contains(calls[0].body, "response_format") || !strings.Contains(calls[0].body, `"parallel_tool_calls":false`) {
		t.Fatalf("Bedrock: calls=%+v", calls)
	}
}

// Routing weighs a request against the cached catalog, so a row the facade
// refreshes can still put a model behind adaptation, which would drop a
// field the client set. The facade refuses such a request before anything
// is sent, as a 400 a route moves past, and serves a request translated
// from another surface as before.
func TestOpenAICompatibleRefusesAFieldAdaptationWouldDrop(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	messages := []Message{{"role": "user", "content": "hi"}}
	forced := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, ForceApiSupport: true})
	storeOpenAICatalog(forced, ModelInfo{ID: "responses-model", SupportedSurfaces: []string{"/responses"}})

	client := Kwargs{ChatFieldsAsSent: true, "response_format": map[string]any{"type": "json_object"}, "temperature": 0.2}
	_, completeErr := forced.Complete("responses-model", messages, client)
	_, streamErr := forced.Stream("responses-model", messages, client)
	for _, err := range []error{completeErr, streamErr} {
		var refusal *InvocationError
		if !errors.As(err, &refusal) || refusal.Status != http.StatusBadRequest || !InvocationFailoverEligible(err) ||
			!strings.Contains(err.Error(), `"response_format"`) {
			t.Fatalf("err=%v, want a 400 naming response_format", err)
		}
	}
	if calls := upstream.take(); len(calls) != 0 {
		t.Fatalf("a refused request reached the upstream: %+v", calls)
	}

	if _, err := forced.Complete("responses-model", messages, Kwargs{ChatFieldsAsSent: true, "temperature": 0.2, "parallel_tool_calls": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := forced.Complete("responses-model", messages, Kwargs{"stream_options": map[string]any{"include_usage": true}}); err != nil {
		t.Fatal(err)
	}
	if calls := upstream.take(); len(calls) != 2 || calls[0].path != "/responses" || calls[1].path != "/responses" {
		t.Fatalf("calls=%+v", calls)
	}
}
