package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// openAICall is one request a synthetic OpenAI-compatible upstream received.
type openAICall struct {
	path, authorization, accept, vision string
	body                                string
}

// openAIUpstream is a synthetic OpenAI-compatible upstream. It records every
// request and answers with answer, or with a Chat completion or a Responses
// response, streamed when the request streams.
type openAIUpstream struct {
	mu     sync.Mutex
	calls  []openAICall
	answer func(w http.ResponseWriter, call openAICall)
}

func newOpenAIUpstream(t *testing.T) (*openAIUpstream, string) {
	t.Helper()
	upstream := &openAIUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	return upstream, server.URL
}

func (u *openAIUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := openAICall{
		path: r.URL.Path, authorization: r.Header.Get("Authorization"), accept: r.Header.Get("Accept"),
		vision: r.Header.Get("Copilot-Vision-Request"), body: string(body),
	}
	u.mu.Lock()
	u.calls = append(u.calls, call)
	answer := u.answer
	u.mu.Unlock()
	if answer != nil {
		answer(w, call)
		return
	}
	answerOpenAI(w, call)
}

// answerOpenAI answers a Chat completion or a Responses response with "ok",
// streamed when the request streams.
func answerOpenAI(w http.ResponseWriter, call openAICall) {
	stream := strings.Contains(call.body, `"stream":true`)
	responses := strings.HasSuffix(call.path, "/responses")
	switch {
	case responses && stream:
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n")
	case responses:
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	case stream:
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	default:
		_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
}

func (u *openAIUpstream) setAnswer(answer func(w http.ResponseWriter, call openAICall)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answer = answer
}

// take returns the requests received since the last take.
func (u *openAIUpstream) take() []openAICall {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls = nil
	return calls
}

// openAIRuntimeCall sends one request on surface for caller on instance
// through the core Runtime, as the facade names an operation of vertical.
func openAIRuntimeCall(runtime *Runtime, vertical string, caller core.Caller, instance string, surface core.ModelSurface, model string) error {
	ctx := withCoreOperation(context.Background(), vertical, caller)
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	if surface == core.ModelSurfaceResponses {
		body = `{"input":"hi"}`
	}
	_, err := runtime.core.Invoke(ctx, caller, instance, core.Request{
		Surface: surface, Model: model, ContentType: core.ContentTypeJSON, Body: []byte(body),
	})
	return err
}

// The core Runtime serves an OpenAI-compatible or a Bedrock instance with
// the key the provider factory resolves for it, in the factory's order: the
// caller's connection, then the system connection, then the configured key,
// an environment reference resolved. Each request resolves its own,
// normalized as the transport normalized a bearer key, and without any the
// request carries no Authorization.
func TestOpenAIVerticalsResolveTheFactoryPrecedence(t *testing.T) {
	for vertical, cfg := range map[string]*config.ProviderConfig{
		openAICompatibleCoreType: {Type: "openai_compatible"},
		bedrockCoreType:          {Type: "bedrock", Region: "us-east-1"},
	} {
		t.Run(vertical, func(t *testing.T) {
			setupCodexProviderTest(t)
			runtime := InstallForTests(t)
			upstream, base := newOpenAIUpstream(t)
			cfg.BaseURL = base
			config.Update(func(s *config.Settings) { s.Providers = map[string]*config.ProviderConfig{"fixture": cfg} })
			human, err := iam.CreatePrincipal("human", "authentik:"+vertical+"-owner", "", "Owner")
			if err != nil {
				t.Fatal(err)
			}
			owner := core.Caller{ID: human.ID, Kind: core.CallerHuman}
			expect := func(caller core.Caller, want string) {
				t.Helper()
				err := openAIRuntimeCall(runtime, vertical, caller, "fixture", core.ModelSurfaceChatCompletions, "chat-model")
				if calls := upstream.take(); err != nil || len(calls) != 1 || calls[0].authorization != want || calls[0].path != "/chat/completions" {
					t.Fatalf("calls=%+v err=%v, want the authorization %q", calls, err, want)
				}
			}
			expect(owner, "")
			t.Setenv("LLMGW_OPENAI_FIXTURE_KEY", "env-key")
			for key, want := range map[string]string{
				"configured-key": "Bearer configured-key", " Bearer configured-key ": "Bearer configured-key",
				"${ENV:LLMGW_OPENAI_FIXTURE_KEY}": "Bearer env-key", "free": "", "NONE": "", "public": "Bearer public",
			} {
				config.Update(func(s *config.Settings) { s.Providers["fixture"].APIKey = key })
				expect(owner, want)
			}
			if _, err := iam.PutSystemProviderConnection("fixture", "api_key", "system-key"); err != nil {
				t.Fatal(err)
			}
			expect(owner, "Bearer system-key")
			connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
				PrincipalID: human.ID, ProviderID: "fixture", Kind: "api_key", Secret: "personal-key", MakeDefault: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			expect(owner, "Bearer personal-key")
			expect(gatewayCaller(), "Bearer system-key")
			if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
				t.Fatal(err)
			}
			expect(owner, "Bearer system-key")

			// A connection of another kind is refused as the factory refused
			// it, and nothing is sent with it.
			if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
				PrincipalID: human.ID, ProviderID: "fixture", Kind: "openai_codex_oauth", MakeDefault: true,
				AccessToken: "oauth-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
			}); err != nil {
				t.Fatal(err)
			}
			err = openAIRuntimeCall(runtime, vertical, owner, "fixture", core.ModelSurfaceChatCompletions, "chat-model")
			var refusal *ConfigError
			if !errors.As(err, &refusal) || refusal.Msg != vertical+": the resolved connection is not an API key" || len(upstream.take()) != 0 {
				t.Fatalf("an OAuth connection: err=%v", err)
			}
		})
	}
}

// Core serves Responses natively for a model the operation's caller's cached
// catalog lists it for, and for every model of the openai entry, and refuses
// any other before anything is sent. Pollinations takes Chat under /v1.
func TestOpenAIVerticalRoutesByTheCallersCatalog(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	upstream, base := newOpenAIUpstream(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"fixture":      {Type: "openai_compatible", BaseURL: base},
			"official":     {Type: "openai_compatible", RegistryID: "openai", BaseURL: base},
			"pollinations": {Type: "openai_compatible", RegistryID: "pollinations", BaseURL: base},
		}
	})
	owner := core.Caller{ID: "prn_catalog_owner", Kind: core.CallerHuman}
	runtime.catalogs.store(catalogCacheKey("fixture", gatewayCaller()), []ModelInfo{
		{ID: "responses-model", SupportedSurfaces: []string{"/chat/completions", "/responses"}},
		{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
	})
	runtime.catalogs.store(catalogCacheKey("fixture", owner), []ModelInfo{{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}}})
	send := func(vertical string, caller core.Caller, instance, model, want string) {
		t.Helper()
		err := openAIRuntimeCall(runtime, vertical, caller, instance, core.ModelSurfaceResponses, model)
		calls := upstream.take()
		var surface *core.SurfaceError
		switch {
		case want == "" && (!errors.As(err, &surface) || len(calls) != 0):
			t.Fatalf("%s %s: err=%v calls=%+v, want a refusal before sending", instance, model, err, calls)
		case want != "" && (err != nil || len(calls) != 1 || calls[0].path != want):
			t.Fatalf("%s %s: err=%v calls=%+v, want %s", instance, model, err, calls, want)
		}
	}
	send(openAICompatibleCoreType, gatewayCaller(), "fixture", "responses-model", "/responses")
	send(openAICompatibleCoreType, gatewayCaller(), "fixture", "chat-model", "")
	send(openAICompatibleCoreType, owner, "fixture", "responses-model", "")
	send(openAICompatibleCoreType, owner, "official", "future-model", "/responses")

	err := openAIRuntimeCall(runtime, openAICompatibleCoreType, gatewayCaller(), "pollinations", core.ModelSurfaceChatCompletions, "openai-fast")
	if calls := upstream.take(); err != nil || len(calls) != 1 || calls[0].path != "/v1/chat/completions" || calls[0].authorization != "" {
		t.Fatalf("pollinations: err=%v calls=%+v", err, calls)
	}
}

// A Bedrock instance is served at its base URL, or at its region's endpoint,
// which is refused unless the region is a region name, before anything is
// sent. Its requests time out as configured.
func TestBedrockVerticalServesItsEndpoint(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	upstream, base := newOpenAIUpstream(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	timeout := 0.2
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"bedrock": {Type: "bedrock", Region: "us-east-1", BaseURL: base + "/", APIKey: "fixture-key"},
			"slow":    {Type: "bedrock", BaseURL: base + "/slow", Timeout: &timeout},
			"evil":    {Type: "bedrock", Region: "evil.example.test/steal#"},
		}
	})
	if err := openAIRuntimeCall(runtime, bedrockCoreType, gatewayCaller(), "bedrock", core.ModelSurfaceChatCompletions, "fixture-model"); err != nil {
		t.Fatal(err)
	}
	if calls := upstream.take(); len(calls) != 1 || calls[0].path != "/chat/completions" || calls[0].authorization != "Bearer fixture-key" {
		t.Fatalf("calls=%+v", calls)
	}
	err := openAIRuntimeCall(runtime, bedrockCoreType, gatewayCaller(), "evil", core.ModelSurfaceChatCompletions, "fixture-model")
	if !IsConfig(err) || !strings.Contains(err.Error(), "not an AWS region name") {
		t.Fatalf("a region that is no region name: err=%v", err)
	}
	upstream.setAnswer(func(w http.ResponseWriter, call openAICall) {
		if strings.HasPrefix(call.path, "/slow") {
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		answerOpenAI(w, call)
	})
	started := time.Now()
	err = openAIRuntimeCall(runtime, bedrockCoreType, gatewayCaller(), "slow", core.ModelSurfaceChatCompletions, "fixture-model")
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("err=%v after %s, want the configured timeout", err, time.Since(started))
	}
}
