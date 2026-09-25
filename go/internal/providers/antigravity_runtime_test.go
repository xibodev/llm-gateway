package providers

import (
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

var antigravityHello = []Message{{"role": "user", "content": "hello"}}

// rejectAntigravityTokensBut answers every Cloud Code Assist call that does
// not carry accepted with a 401.
func rejectAntigravityTokensBut(accepted string) func(http.ResponseWriter, *http.Request) bool {
	return func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1internal:") && r.Header.Get("Authorization") != "Bearer "+accepted {
			w.WriteHeader(http.StatusUnauthorized)
			return true
		}
		return false
	}
}

func antigravityCallsWithoutBodies(calls []antigravityCall) []antigravityCall {
	for index := range calls {
		calls[index].body = ""
	}
	return calls
}

// A connection whose sign-in discovered no project gets the one its first
// request discovers, with a write that keeps its tokens, and later requests
// send that project without discovering it again.
func TestAntigravityChatStoresTheDiscoveredProject(t *testing.T) {
	upstream := &antigravityUpstream{}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "current-access", "refresh-token", "", 0)
	for range 2 {
		if _, err := provider.Complete("model-a", antigravityHello, nil); err != nil {
			t.Fatal(err)
		}
	}
	envelope, _ := storedAntigravityConnection(t, owner)
	if envelope.ProjectID != "discovered-project" || envelope.AccessToken != "current-access" || envelope.RefreshToken != "refresh-token" {
		t.Fatalf("project=%q, tokens kept=%v", envelope.ProjectID, envelope.AccessToken == "current-access" && envelope.RefreshToken == "refresh-token")
	}
	streams := upstream.to("/v1internal:streamGenerateContent")
	if upstream.count("/v1internal:loadCodeAssist") != 1 || upstream.count("/token") != 0 || len(streams) != 2 {
		t.Fatalf("calls=%+v", upstream.recorded())
	}
	for _, stream := range streams {
		if project := antigravityBodyField(t, stream.body, "project"); project != "discovered-project" {
			t.Fatalf("project=%v", project)
		}
	}
}

// An upstream 401 makes the core Runtime refresh the connection once, keep
// the project the refreshed token discovers, and replay the request.
func TestAntigravityChatRefreshesARejectedTokenAndReplays(t *testing.T) {
	upstream := &antigravityUpstream{handle: rejectAntigravityTokensBut("refreshed-access")}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "rejected-access", "refresh-token", "project", time.Now().Add(time.Hour).Unix())
	response, err := provider.Complete("model-a", antigravityHello, nil)
	if choices, _ := response["choices"].([]any); err != nil || len(choices) != 1 {
		t.Fatalf("response=%v err=%v", response, err)
	}
	want := []antigravityCall{
		{path: "/v1internal:streamGenerateContent", authorization: "Bearer rejected-access"},
		{path: "/token"},
		{path: "/load", authorization: "Bearer refreshed-access"},
		{path: "/v1internal:streamGenerateContent", authorization: "Bearer refreshed-access"},
	}
	if calls := antigravityCallsWithoutBodies(upstream.recorded()); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%+v, want %+v", calls, want)
	}
	envelope, _ := storedAntigravityConnection(t, owner)
	if envelope.AccessToken != "refreshed-access" || envelope.RefreshToken != "refresh-token" || envelope.ProjectID != "discovered-project" ||
		envelope.OAuthProfile != antigravityOAuthProfileRuntimeSecret || envelope.OAuthClientID != "client-id" {
		t.Fatalf("stored project=%q profile=%q", envelope.ProjectID, envelope.OAuthProfile)
	}
}

// When the refresh after a 401 fails, the Runtime returns the 401. The
// Antigravity path reported its failed recovery as a failed completion
// without a status, which routing may fail over from, and never revoked the
// connection, however final the token endpoint's refusal.
func TestAntigravityChatReportsAFailedRecoveryWithoutAStatus(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/token":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		case "/v1internal:streamGenerateContent":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			return false
		}
		return true
	}}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "rejected-access", "refresh-token", "project", time.Now().Add(time.Hour).Unix())
	_, err := provider.Complete("model-a", antigravityHello, nil)
	if !IsInvocation(err) || UpstreamStatus(err) != 0 || !InvocationFailoverEligible(err) || err.Error() != "google_antigravity: completion failed" {
		t.Fatalf("err=%v status=%d", err, UpstreamStatus(err))
	}
	if streams, refreshes := upstream.count("/v1internal:streamGenerateContent"), upstream.count("/token"); streams != 1 || refreshes != 1 {
		t.Fatalf("streams=%d refreshes=%d", streams, refreshes)
	}
	if envelope, _ := storedAntigravityConnection(t, owner); envelope.AccessToken != "rejected-access" {
		t.Fatal("the failed refresh replaced the connection")
	}
}

// A replay the upstream rejects again keeps its 401, as the Antigravity
// path's replay did.
func TestAntigravityChatReportsARejectedReplay(t *testing.T) {
	upstream := &antigravityUpstream{handle: rejectAntigravityTokensBut("never-accepted")}
	provider, _ := setupAntigravityRuntimeTest(t, upstream, "rejected-access", "refresh-token", "project", time.Now().Add(time.Hour).Unix())
	_, err := provider.Complete("model-a", antigravityHello, nil)
	if UpstreamStatus(err) != http.StatusUnauthorized || InvocationFailoverEligible(err) {
		t.Fatalf("err=%v status=%d", err, UpstreamStatus(err))
	}
	if streams := upstream.count("/v1internal:streamGenerateContent"); streams != 2 {
		t.Fatalf("streams=%d, want the request and its replay", streams)
	}
}

// A caller without a connection of their own fails before anything is sent,
// with the failed completion the Antigravity path reported.
func TestAntigravityChatWithoutConnectionFailsBeforeUpstream(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		t.Errorf("a request without a connection reached %s", r.URL.Path)
		return false
	}}
	setupAntigravityRuntimeTest(t, upstream, "owner-access", "refresh-token", "project", 0)
	stranger, err := iam.CreatePrincipal("human", "fixture:antigravity-stranger", "", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	_, observation, err := antigravityFacade(t, stranger).CompleteWithObservation("model-a", antigravityHello, nil)
	if !IsInvocation(err) || UpstreamStatus(err) != 0 || observation != nil {
		t.Fatalf("err=%v observation=%+v", err, observation)
	}
}

func TestAntigravityGatewayAdapterCompletesTypedMessages(t *testing.T) {
	upstream := &antigravityUpstream{}
	provider, _ := setupAntigravityRuntimeTest(t, upstream, "access-token", "refresh-token", "project", 0)
	response, err := provider.CompleteContext(context.Background(), "model-a", []Message{{"role": "user", "content": "hi"}}, Kwargs{
		"max_tokens": 32, "temperature": 0.2,
		"_fallback_timeout_ms": 1000, "_affinity_key": "private", "_force_api_support": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if choices, _ := response["choices"].([]any); len(choices) != 1 {
		t.Fatalf("response=%v", response)
	}
	streams := upstream.to("/v1internal:streamGenerateContent")
	if len(streams) != 1 {
		t.Fatalf("calls=%+v", upstream.recorded())
	}
	request, _ := antigravityBodyField(t, streams[0].body, "request").(map[string]any)
	generation, _ := request["generationConfig"].(map[string]any)
	if generation["maxOutputTokens"] != float64(32) || generation["temperature"] != 0.2 {
		t.Fatalf("generationConfig=%v", generation)
	}
	for _, private := range []string{"_fallback_timeout_ms", "_affinity_key", "_force_api_support", "private"} {
		if strings.Contains(streams[0].body, private) {
			t.Fatalf("gateway-private option reached Antigravity: %s", streams[0].body)
		}
	}
}

// Probes attribute their result to the credential revision the operation
// used, as Codex probes do: a completion reports the connection it sent,
// here the one the refresh after a 401 stored.
func TestAntigravityCompletionReportsTheCredentialItUsed(t *testing.T) {
	upstream := &antigravityUpstream{handle: rejectAntigravityTokensBut("refreshed-access")}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "rejected-access", "refresh-token", "project", time.Now().Add(time.Hour).Unix())
	_, observation, err := provider.CompleteWithObservation("model-a", antigravityHello, nil)
	if err != nil {
		t.Fatal(err)
	}
	current, found, err := iam.ActiveProviderAccountObservation(owner.ID, "antigravity")
	if err != nil || !found || observation == nil || *observation != *credentialObservation(&current) || observation.CredentialRevision <= 0 {
		t.Fatalf("observation=%+v current=%+v err=%v", observation, current, err)
	}
	// Serving a request still marks the connection used for the console.
	if connections, err := iam.ListProviderConnections(owner.ID, "antigravity"); err != nil || len(connections) != 1 || connections[0].LastUsedAt == 0 {
		t.Fatalf("connections=%+v err=%v, want the connection marked used", connections, err)
	}
}

// The facade never streams, and sends nothing trying.
func TestAntigravityRefusesToStream(t *testing.T) {
	if _, err := (&antigravityProvider{caller: core.Caller{ID: "owner", Kind: core.CallerHuman}}).Stream("model-a", antigravityHello, nil); !IsConfig(err) {
		t.Fatalf("err=%v", err)
	}
}
