package providers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	core "github.com/xibodev/llmgw-core"
)

// copilotAPICall is one request the synthetic Copilot API received.
type copilotAPICall struct {
	path   string
	header http.Header
	body   string
}

// copilotUpstream is a synthetic GitHub session-token exchange and, on its
// own server, the Copilot API its sessions name as their base. An exchange
// issues a new session for the OAuth token it presents unless refuse answers
// it with a status; the API records each call and answers with answer.
type copilotUpstream struct {
	github, api *httptest.Server

	mu        sync.Mutex
	issued    int
	exchanges []string
	calls     []copilotAPICall
	refuse    func(oauth string) int
	answer    func(w http.ResponseWriter, call copilotAPICall)
}

func newCopilotUpstream(t *testing.T) *copilotUpstream {
	t.Helper()
	upstream := &copilotUpstream{answer: answerCopilot}
	upstream.github = httptest.NewServer(http.HandlerFunc(upstream.exchange))
	upstream.api = httptest.NewServer(http.HandlerFunc(upstream.serve))
	t.Cleanup(upstream.github.Close)
	t.Cleanup(upstream.api.Close)
	return upstream
}

func (u *copilotUpstream) exchange(w http.ResponseWriter, r *http.Request) {
	oauth := strings.TrimPrefix(r.Header.Get("Authorization"), "token ")
	u.mu.Lock()
	u.exchanges = append(u.exchanges, oauth)
	u.issued++
	session, refuse := fmt.Sprintf("%s-session-%d", oauth, u.issued), u.refuse
	u.mu.Unlock()
	if refuse != nil {
		if status := refuse(oauth); status != 0 {
			w.WriteHeader(status)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token": session, "expires_at": time.Now().Add(30 * time.Minute).Unix(),
		"endpoints": map[string]any{"api": u.api.URL},
	})
}

func (u *copilotUpstream) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	call := copilotAPICall{path: r.URL.Path, header: r.Header.Clone(), body: string(body)}
	u.mu.Lock()
	u.calls = append(u.calls, call)
	answer := u.answer
	u.mu.Unlock()
	answer(w, call)
}

// setAnswer replaces how the API answers.
func (u *copilotUpstream) setAnswer(answer func(w http.ResponseWriter, call copilotAPICall)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answer = answer
}

// setRefuse replaces how the exchange answers.
func (u *copilotUpstream) setRefuse(refuse func(oauth string) int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.refuse = refuse
}

// take returns the exchanged OAuth tokens and the API calls since the last
// take.
func (u *copilotUpstream) take() ([]string, []copilotAPICall) {
	u.mu.Lock()
	defer u.mu.Unlock()
	exchanges, calls := u.exchanges, u.calls
	u.exchanges, u.calls = nil, nil
	return exchanges, calls
}

// copilotCatalogFixture lists a Chat model, a Chat model with reasoning
// efforts and a model that lists only Responses.
const copilotCatalogFixture = `{"data":[` +
	`{"id":"chat-model","vendor":"Fixture","supported_endpoints":["/chat/completions"]},` +
	`{"id":"reasoning-model","supported_endpoints":["/chat/completions"],"capabilities":{"supports":{"reasoning_effort":["low","high"]}}},` +
	`{"id":"responses-model","supported_endpoints":["/responses"]}]}`

const (
	copilotChatFixture      = `{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`
	copilotResponsesFixture = `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	copilotChatStream       = "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"}}]}\n\ndata: [DONE]\n\n"
	copilotResponsesStream  = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"
)

// answerCopilot answers as Copilot does: the catalog, and a Chat or
// Responses answer, streamed when the request streams.
func answerCopilot(w http.ResponseWriter, call copilotAPICall) {
	streamed := strings.Contains(call.body, `"stream":true`)
	switch {
	case call.path == "/models":
		_, _ = io.WriteString(w, copilotCatalogFixture)
	case call.path == "/chat/completions" && streamed:
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, copilotChatStream)
	case call.path == "/chat/completions":
		_, _ = io.WriteString(w, copilotChatFixture)
	case call.path == "/responses" && streamed:
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, copilotResponsesStream)
	case call.path == "/responses":
		_, _ = io.WriteString(w, copilotResponsesFixture)
	default:
		http.NotFound(w, nil)
	}
}

// installCopilot installs a Runtime whose instance "copilot", configured as
// cfg, reaches upstream, with the gateway-wide token fixture-oauth and a
// synthetic editor identity.
func installCopilot(t *testing.T, upstream *copilotUpstream, cfg *config.ProviderConfig) *Runtime {
	t.Helper()
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	runtime.copilotEndpoints.swap(copilotauth.Endpoints{SessionTokenURL: upstream.github.URL})
	previous := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.GithubCopilotIntegrationID, s.GithubCopilotEditorVersion = previous.GithubCopilotIntegrationID, previous.GithubCopilotEditorVersion
		})
	})
	withCopilotSettings(t, func(s *config.Settings) {
		s.GithubCopilotCacheDir, s.GithubCopilotOAuthToken = t.TempDir(), "fixture-oauth"
		s.GithubCopilotUseGhCLI, s.AllowCopilotProxy = false, true
		s.GithubCopilotIntegrationID, s.GithubCopilotEditorVersion = "fixture-integration", "fixture-editor/1.0"
		s.Providers = map[string]*config.ProviderConfig{"copilot": cfg}
	})
	return runtime
}

// copilotFacade returns the facade the factory builds for caller.
func copilotFacade(t *testing.T, runtime *Runtime, caller core.Caller) *copilotProvider {
	t.Helper()
	provider, err := runtime.GetProviderForPrincipal("copilot", caller)
	if err != nil {
		t.Fatal(err)
	}
	copilot, ok := provider.(*copilotProvider)
	if !ok {
		t.Fatalf("provider=%T", provider)
	}
	return copilot
}

// copilotTransportBody encodes payload as the OpenAI transport encoded a
// request body.
func copilotTransportBody(payload map[string]any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
	return strings.TrimRight(buffer.String(), "\n")
}

// assertCopilotHeaders checks the Copilot headers the OpenAI transport sent.
func assertCopilotHeaders(t *testing.T, call copilotAPICall, session, accept string, vision bool) {
	t.Helper()
	for name, want := range map[string]string{
		"Authorization": "Bearer " + session, "Content-Type": "application/json", "Accept": accept,
		"Copilot-Integration-Id": "fixture-integration", "Editor-Version": "fixture-editor/1.0",
		"Editor-Plugin-Version": "llm-gateway/0.1", "OpenAI-Intent": "conversation-panel",
		"User-Agent": "GithubCopilotChat/llm-gateway",
	} {
		if got := call.header.Get(name); got != want {
			t.Fatalf("%s %s: %s=%q, want %q", call.path, call.body, name, got, want)
		}
	}
	if got := call.header.Get("Copilot-Vision-Request") == "true"; got != vision {
		t.Fatalf("%s: vision header=%v, want %v", call.path, got, vision)
	}
}

// Chat reaches Copilot as the OpenAI transport sent it: the payload it built,
// the fields core does not carry included, with the editor identity and the
// vision header. The provider lists the catalog it routes by once for the
// credential, renames max_tokens for a reasoning model and serves a model that
// lists only Responses over Responses, marked as the transport marked it.
func TestCopilotChatSendsTheTransportsRequest(t *testing.T) {
	upstream := newCopilotUpstream(t)
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"}), gatewayCaller())
	messages := []Message{{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "Say <b>hi</b> & more"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}},
	}}}
	kw := Kwargs{
		"temperature": 0.5, "top_p": 1.0, "max_tokens": float64(64), "stop": []any{"END"},
		"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}},
		"tool_choice": "auto", "metadata": map[string]any{"client": "fixture"}, "reasoning_effort": "low",
		"parallel_tool_calls": false, "stream_options": map[string]any{"include_usage": true},
		"thinking":      map[string]any{"type": "enabled", "budget_tokens": float64(1024)},
		"output_config": map[string]any{"effort": "low"}, "_fallback_timeout_ms": 5000, "_affinity_key": "fixture",
	}
	if _, err := provider.Complete("chat-model", messages, kw); err != nil {
		t.Fatal(err)
	}
	exchanges, calls := upstream.take()
	if len(exchanges) != 1 || exchanges[0] != "fixture-oauth" || len(calls) != 2 || calls[0].path != "/models" {
		t.Fatalf("exchanges=%v calls=%+v, want the catalog listed and then the request", exchanges, calls)
	}
	want := copilotTransportBody(buildOpenAIPayload("chat-model", messages, false, kw))
	if calls[1].path != "/chat/completions" || calls[1].body != want {
		t.Fatalf("upstream %s body=%s\nwant %s", calls[1].path, calls[1].body, want)
	}
	for _, field := range []string{`"parallel_tool_calls":false`, `"stream_options":{"include_usage":true}`, `"budget_tokens":1024`, "<b>hi</b> & more"} {
		if !strings.Contains(calls[1].body, field) {
			t.Fatalf("body=%s lacks %s", calls[1].body, field)
		}
	}
	assertCopilotHeaders(t, calls[1], "fixture-oauth-session-1", "application/json", true)

	plain := []Message{{"role": "user", "content": "hi"}}
	for _, fixture := range []struct {
		model  string
		kw     Kwargs
		path   string
		body   string
		marked bool
	}{
		{"reasoning-model", Kwargs{"max_tokens": float64(64)}, "/chat/completions",
			copilotTransportBody(buildOpenAIPayload("reasoning-model", plain, false, Kwargs{"max_completion_tokens": float64(64)})), false},
		{"responses-model", Kwargs{"_max_output_tokens": float64(32), "temperature": 0.2}, "/responses",
			copilotTransportBody(chatToResponsesWithReport("responses-model", plain, Kwargs{"max_completion_tokens": float64(32), "temperature": 0.2}).Value), true},
		{"responses-model", Kwargs{"_force_api_support": false}, "/chat/completions",
			copilotTransportBody(buildOpenAIPayload("responses-model", plain, false, nil)), false},
	} {
		response, err := provider.Complete(fixture.model, plain, fixture.kw)
		if err != nil {
			t.Fatalf("%s %v: %v", fixture.model, fixture.kw, err)
		}
		_, calls := upstream.take()
		if len(calls) != 1 || calls[0].path != fixture.path || calls[0].body != fixture.body {
			t.Fatalf("%s %v: calls=%+v\nwant %s %s", fixture.model, fixture.kw, calls, fixture.path, fixture.body)
		}
		if _, marked := response["forced_support"]; marked != fixture.marked {
			t.Fatalf("%s %v: response=%v, marked=%v", fixture.model, fixture.kw, response, fixture.marked)
		}
	}

	// A request Responses cannot carry is refused before it is sent.
	if _, err := provider.Complete("responses-model", plain, Kwargs{"stop": []any{"END"}}); !IsConfig(err) || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("material conversion: err=%v", err)
	}
	if _, calls := upstream.take(); len(calls) != 0 {
		t.Fatalf("material conversion sent %+v", calls)
	}
	stream, err := provider.Stream("chat-model", plain, Kwargs{"stream_options": map[string]any{"include_usage": true}})
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if _, calls := upstream.take(); len(calls) != 1 ||
		calls[0].body != copilotTransportBody(buildOpenAIPayload("chat-model", plain, true, Kwargs{"stream_options": map[string]any{"include_usage": true}})) {
		t.Fatalf("stream calls=%+v", calls)
	}
}

func drainCopilotStream(t *testing.T, stream StreamIter, err error) ([]string, error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, ok := stream.Next()
		if !ok {
			return chunks, stream.Err()
		}
		chunks = append(chunks, chunk)
	}
}

// Responses is native for a model the listed catalog names Responses for, and
// is sent as the transport sent it. For any other model, and for one whose
// Responses endpoint Copilot answers 404, the facade reports
// ErrResponsesUnsupported, on which the router serves the request over Chat.
func TestCopilotResponsesAreNativeOrFallBackToChat(t *testing.T) {
	upstream := newCopilotUpstream(t)
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"}), gatewayCaller())
	payload := map[string]any{"input": "hi", "force_api_support": true, "stream": true}
	response, _, err := provider.CompleteResponses("responses-model", payload)
	if err != nil || response["id"] != "resp_1" {
		t.Fatalf("response=%v err=%v", response, err)
	}
	_, calls := upstream.take()
	if len(calls) != 2 || calls[0].path != "/models" || calls[1].path != "/responses" ||
		calls[1].body != `{"input":"hi","model":"responses-model","stream":false}` {
		t.Fatalf("calls=%+v", calls)
	}
	assertCopilotHeaders(t, calls[1], "fixture-oauth-session-1", "application/json", false)

	stream, _, err := provider.StreamResponses("responses-model", payload)
	chunks, err := drainCopilotStream(t, stream, err)
	if err != nil || len(chunks) != 1 || !strings.Contains(chunks[0], `"response.completed"`) {
		t.Fatalf("chunks=%q err=%v", chunks, err)
	}
	if _, calls = upstream.take(); len(calls) != 1 || calls[0].body != `{"input":"hi","model":"responses-model","stream":true}` {
		t.Fatalf("stream calls=%+v", calls)
	}
	assertCopilotHeaders(t, calls[0], "fixture-oauth-session-1", "text/event-stream", false)

	for _, model := range []string{"chat-model", "unlisted-model"} {
		if _, _, err := provider.CompleteResponses(model, payload); err != ErrResponsesUnsupported {
			t.Fatalf("%s: err=%v", model, err)
		}
		if _, _, err := provider.StreamResponses(model, payload); err != ErrResponsesUnsupported {
			t.Fatalf("%s stream: err=%v", model, err)
		}
	}
	if _, calls = upstream.take(); len(calls) != 0 {
		t.Fatalf("a model without native Responses sent %+v", calls)
	}
	upstream.setAnswer(func(w http.ResponseWriter, call copilotAPICall) { http.NotFound(w, nil) })
	if _, _, err := provider.CompleteResponses("responses-model", payload); err != ErrResponsesUnsupported {
		t.Fatalf("unrouted Responses: err=%v", err)
	}
	if _, calls = upstream.take(); len(calls) != 1 {
		t.Fatalf("unrouted Responses calls=%+v", calls)
	}
}

// Core relays Copilot's records as sent; the facade hands the API layer their
// data as the transport's stream did, skipping comments and [DONE].
func TestCopilotStreamsRelayDataEvents(t *testing.T) {
	const first = `{"id":"c","choices":[{"index":0,"delta":{"content":"a"}}]}`
	const second = `{"id":"c","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}`
	upstream := newCopilotUpstream(t)
	upstream.setAnswer(func(w http.ResponseWriter, _ copilotAPICall) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ": keepalive\n\ndata: "+first+"\n\nevent: message\ndata: "+second+"\r\n\r\ndata: [DONE]\n\n")
	})
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"}), gatewayCaller())
	stream, err := provider.Stream("chat-model", []Message{{"role": "user", "content": "hi"}}, Kwargs{"_force_api_support": false})
	chunks, err := drainCopilotStream(t, stream, err)
	if err != nil || strings.Join(chunks, "|") != first+"|"+second {
		t.Fatalf("chunks=%q err=%v", chunks, err)
	}
}

// Each request resolves the caller's GitHub token as the resolver did: a
// human's own connection, then their legacy credential, a service's or a
// system principal's project binding, and the gateway-wide token for a
// caller without a principal. The facade reports the resolver's observation.
// A principal without a credential is refused before anything is sent,
// never served with the gateway's token.
func TestCopilotRequestsResolveTheResolversCredential(t *testing.T) {
	upstream := newCopilotUpstream(t)
	runtime := installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"})
	check := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	project, err := iam.CreateProject("copilot-project", "Copilot")
	check(err)
	systemProject, err := iam.CreateProject("copilot-system", "Copilot System")
	check(err)
	member := func(project iam.Project, kind, subject string) iam.Principal {
		t.Helper()
		principal, err := iam.CreatePrincipal(kind, subject, "", subject)
		check(err)
		check(iam.SetMembership(project.ID, principal.ID, "member"))
		return principal
	}
	owner, legacy := member(project, "human", "authentik:owner"), member(project, "human", "authentik:legacy")
	service, stranger := member(project, "service", "service:copilot"), member(project, "human", "authentik:stranger")
	_, err = iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "copilot", Kind: "github_oauth", Source: iam.ConnectionSourceUser,
		MakeDefault: true, AccessToken: "owner-oauth",
	})
	check(err)
	_, err = iam.PutProviderCredential(legacy.ID, "copilot", "github_oauth", "legacy-oauth")
	check(err)
	// The gateway owns one credential per provider. The project binds it for
	// services, and the system project for system principals.
	shared, err := iam.PutGatewayProviderCredential("copilot", "github_oauth", "shared-oauth")
	check(err)
	_, err = iam.SetProviderCredentialBinding(project.ID, "copilot", "service", shared.ID)
	check(err)
	system, err := iam.EnsureSystemPrincipal()
	check(err)
	check(iam.SetMembership(project.ID, system.ID, "member"))
	check(iam.SetMembership(systemProject.ID, system.ID, "member"))
	// The binding writer issues only service bindings; the resolver honours
	// a system one, so the fixture writes it directly.
	db, err := iam.DB()
	check(err)
	_, err = db.Exec(`INSERT INTO provider_credential_bindings(project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at)
VALUES(?,?,?,?,'active',?,?)`, systemProject.ID, "copilot", "system", shared.ID, time.Now().Unix(), time.Now().Unix())
	check(err)
	systemCaller := func(project iam.Project) core.Caller {
		return core.Caller{ID: iam.SystemPrincipalCallerID(system.ID), Kind: core.CallerService, ProjectID: project.ID}
	}

	plain := []Message{{"role": "user", "content": "hi"}}
	for _, fixture := range []struct {
		name   string
		caller core.Caller
		token  string
	}{
		{"own connection", core.Caller{ID: owner.ID, Kind: core.CallerHuman, ProjectID: project.ID}, "owner-oauth"},
		{"legacy credential", core.Caller{ID: legacy.ID, Kind: core.CallerHuman}, "legacy-oauth"},
		{"service binding", core.Caller{ID: service.ID, Kind: core.CallerService, ProjectID: project.ID}, "shared-oauth"},
		{"system binding", systemCaller(systemProject), "shared-oauth"},
		{"gateway-wide token", gatewayCaller(), "fixture-oauth"},
	} {
		_, observation, err := copilotFacade(t, runtime, fixture.caller).CompleteWithObservation("chat-model", plain, Kwargs{"_force_api_support": false})
		// A token's session serves every request that token authorizes.
		exchanges, calls := upstream.take()
		if err != nil || len(exchanges) > 1 || (len(exchanges) == 1 && exchanges[0] != fixture.token) || len(calls) != 1 ||
			!strings.HasPrefix(calls[0].header.Get("Authorization"), "Bearer "+fixture.token+"-session-") {
			t.Fatalf("%s: exchanges=%v calls=%+v err=%v, want %s", fixture.name, exchanges, calls, err, fixture.token)
		}
		secret, reference, found, err := iam.ResolveCallerOAuthCredentialSecretWithObservation(fixture.caller, "copilot")
		if err != nil || (found && secret != fixture.token) || fmt.Sprint(observation) != fmt.Sprint(credentialObservation(reference)) {
			t.Fatalf("%s: observation=%v, want the resolver's %v (found=%v err=%v)", fixture.name, observation, reference, found, err)
		}
		if fixture.name == "own connection" && observation == nil {
			t.Fatalf("%s: the personal connection reported no observation", fixture.name)
		}
	}

	// A binding serves only its kind, so the system principal has no
	// credential in the project that binds one for services. The instance
	// cache keys a system principal without its project, so each caller gets
	// a facade of its own.
	for _, caller := range []core.Caller{{ID: stranger.ID, Kind: core.CallerHuman}, systemCaller(project)} {
		runtime.ResetProviders()
		_, err = copilotFacade(t, runtime, caller).Complete("chat-model", plain, nil)
		if !IsConfig(err) || err.Error() != "github_copilot: this principal has no active Copilot credential" {
			t.Fatalf("%s without a credential: err=%v", caller.ID, err)
		}
		if exchanges, calls := upstream.take(); len(exchanges) != 0 || len(calls) != 0 {
			t.Fatalf("%s without a credential reached Copilot: exchanges=%v calls=%+v", caller.ID, exchanges, calls)
		}
	}
}

// Copilot's refusals keep the status, Retry-After and dispositions the router
// and the resilience wrapper decided on before. A session Copilot rejects is
// replaced once and the request replayed, its vision header kept; a second
// rejection is final.
func TestCopilotFailuresKeepTheirClassification(t *testing.T) {
	upstream := newCopilotUpstream(t)
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"}), gatewayCaller())
	var mu sync.Mutex
	statuses := []int{}
	upstream.setAnswer(func(w http.ResponseWriter, call copilotAPICall) {
		mu.Lock()
		status := http.StatusOK
		if len(statuses) > 0 {
			status, statuses = statuses[0], statuses[1:]
		}
		mu.Unlock()
		if status == http.StatusOK {
			answerCopilot(w, call)
			return
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"refused","code":"fixture_code"}}`)
	})
	answer := func(sequence ...int) {
		mu.Lock()
		defer mu.Unlock()
		statuses = sequence
	}
	vision := []Message{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}}}}
	chat := func() error {
		_, err := provider.Complete("chat-model", vision, Kwargs{"_force_api_support": false})
		return err
	}
	for _, fixture := range []struct {
		status                     int
		retryAfter                 string
		retryable, failover, trips bool
	}{
		{http.StatusTooManyRequests, "7", true, true, true},
		{http.StatusServiceUnavailable, "7", true, true, true},
		{http.StatusBadRequest, "7", false, false, false},
		{http.StatusForbidden, "", false, false, false},
	} {
		answer(fixture.status)
		err := chat()
		if UpstreamStatus(err) != fixture.status || InvocationRetryAfter(err) != fixture.retryAfter ||
			InvocationRetryable(err) != fixture.retryable || InvocationFailoverEligible(err) != fixture.failover ||
			InvocationCircuitFailure(err) != fixture.trips || strings.Contains(err.Error(), "refused") {
			t.Fatalf("status %d: err=%v retry-after=%q", fixture.status, err, InvocationRetryAfter(err))
		}
		if throttle := fixture.status == http.StatusTooManyRequests; IsThrottle(err) != throttle {
			t.Fatalf("status %d: throttle=%v, want %v", fixture.status, IsThrottle(err), throttle)
		}
	}
	upstream.take()

	answer(http.StatusUnauthorized)
	if err := chat(); err != nil {
		t.Fatalf("a replaced session: %v", err)
	}
	exchanges, calls := upstream.take()
	if len(exchanges) != 1 || len(calls) != 2 {
		t.Fatalf("a rejected session: exchanges=%v calls=%+v, want one new session and a replay", exchanges, calls)
	}
	assertCopilotHeaders(t, calls[0], "fixture-oauth-session-1", "application/json", true)
	assertCopilotHeaders(t, calls[1], "fixture-oauth-session-2", "application/json", true)

	answer(http.StatusUnauthorized, http.StatusUnauthorized)
	err := chat()
	if UpstreamStatus(err) != http.StatusUnauthorized || InvocationRetryable(err) || InvocationFailoverEligible(err) {
		t.Fatalf("a second rejection: err=%v retry=%v failover=%v", err, InvocationRetryable(err), InvocationFailoverEligible(err))
	}
	if _, calls = upstream.take(); len(calls) != 2 {
		t.Fatalf("a second rejection: calls=%d, want 2", len(calls))
	}
}

// The catalog stays on the gateway's path: the facade lists Copilot's
// /models with a session for its caller and reads the rows as the OpenAI
// transport read them, with the untyped capabilities /v1/models presents. A
// session Copilot rejects is replaced once. An exchange that fails reports
// no status, and one that fails after a rejection reports a failed refresh.
func TestCopilotCatalogStaysOnTheGatewayPath(t *testing.T) {
	const catalog = `{"data":[{"id":"gpt-fixture","name":"GPT Fixture","vendor":"Fixture Vendor",` +
		`"supported_endpoints":["/chat/completions","/responses"],"capabilities":{"family":"gpt",` +
		`"limits":{"max_context_window_tokens":128000,"max_output_tokens":4096},` +
		`"supports":{"streaming":true,"tool_calls":true,"vision":true,"reasoning_effort":["low","high"]}}}]}`
	upstream := newCopilotUpstream(t)
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"}), gatewayCaller())
	var mu sync.Mutex
	rejections := 0
	upstream.setAnswer(func(w http.ResponseWriter, _ copilotAPICall) {
		mu.Lock()
		defer mu.Unlock()
		if rejections > 0 {
			rejections--
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, catalog)
	})
	reject := func(count int) {
		mu.Lock()
		defer mu.Unlock()
		rejections = count
	}
	want := fmt.Sprint([]ModelInfo{{
		ID: "gpt-fixture", Vendor: "Fixture Vendor", Label: "GPT Fixture",
		Capabilities: map[string]any{
			"reasoning_effort": []string{"low", "high"}, "streaming": true, "tool_calls": true, "vision": true,
			"context_window": 128000, "max_output_tokens": 4096, "family": "gpt",
		},
		SupportedSurfaces: []string{"/chat/completions", "/responses"},
	}})
	models, observation, err := provider.ListModelsWithError()
	exchanges, calls := upstream.take()
	if err != nil || observation != nil || fmt.Sprint(models) != want || len(exchanges) != 1 || len(calls) != 1 || calls[0].path != "/models" {
		t.Fatalf("models=%v observation=%v err=%v exchanges=%v calls=%+v", models, observation, err, exchanges, calls)
	}
	assertCopilotHeaders(t, calls[0], "fixture-oauth-session-1", "application/json", false)

	reject(1)
	models, _, err = provider.ListModelsWithError()
	if exchanges, calls = upstream.take(); err != nil || fmt.Sprint(models) != want || len(exchanges) != 1 || len(calls) != 2 {
		t.Fatalf("a rejected session: models=%v err=%v exchanges=%v calls=%d", models, err, exchanges, len(calls))
	}
	assertCopilotHeaders(t, calls[1], "fixture-oauth-session-2", "application/json", false)

	upstream.setRefuse(func(string) int { return http.StatusUnauthorized })
	reject(1)
	_, _, err = provider.ListModelsWithError()
	if code, _, status := CatalogFailure(err); code != "catalog_refresh_failed" || status != http.StatusUnauthorized {
		t.Fatalf("a failed refresh: failure=(%q,%d)", code, status)
	}
	// A token without a session exchanges one first. GitHub rejecting it is a
	// 401 core would report, but the catalog reports none, as before.
	config.Update(func(s *config.Settings) { s.GithubCopilotOAuthToken = "fresh-oauth" })
	_, _, err = provider.ListModelsWithError()
	if code, _, status := CatalogFailure(err); code != "catalog_authentication_failed" || status != 0 {
		t.Fatalf("a refused exchange: failure=(%q,%d)", code, status)
	}
}

// The endpoints the gateway proxies to Copilot go to the session's API base
// with the headers the transport sent, and nowhere without a session.
func TestCopilotProxyTargetIsTheSession(t *testing.T) {
	upstream := newCopilotUpstream(t)
	runtime := installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"})
	base, headers, ok := runtime.ProviderHTTPTarget("copilot", gatewayCaller())
	if !ok || base != upstream.api.URL {
		t.Fatalf("target=%q %v ok=%v", base, headers, ok)
	}
	assertCopilotHeaders(t, copilotAPICall{path: "target", header: headers}, "fixture-oauth-session-1", "application/json", false)
	config.Update(func(s *config.Settings) { s.AllowCopilotProxy = false })
	if _, _, ok := runtime.ProviderHTTPTarget("copilot", gatewayCaller()); ok {
		t.Fatal("a disabled Copilot yielded a proxy target")
	}
}

// Core's requests time out as the instance configures, as the transport's
// did.
func TestCopilotRequestsTimeOutAsConfigured(t *testing.T) {
	upstream := newCopilotUpstream(t)
	release := make(chan struct{})
	upstream.setAnswer(func(http.ResponseWriter, copilotAPICall) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(release) })
	timeout := 0.2
	provider := copilotFacade(t, installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot", Timeout: &timeout}), gatewayCaller())
	started := time.Now()
	_, err := provider.Complete("chat-model", []Message{{"role": "user", "content": "hi"}}, Kwargs{"_force_api_support": false})
	if !InvocationRetryable(err) || time.Since(started) > 3*time.Second {
		t.Fatalf("err=%v after %s", err, time.Since(started))
	}
}
