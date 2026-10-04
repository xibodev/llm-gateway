package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llmgw-core/oauthflow"
)

// codexDaemonStart is a Codex sign-in start as the companion daemon receives
// it.
type codexDaemonStart struct {
	Method string            `json:"method"`
	Params map[string]string `json:"params"`
}

// codexSignIn is a gateway whose Codex provider signs in through a fake
// companion daemon, which records every start it receives.
type codexSignIn struct {
	server *httptest.Server
	owner  iam.Principal
	mu     sync.Mutex
	starts []codexDaemonStart
}

// newCodexSignIn serves a gateway whose configured Codex OAuth client ID is
// clientID. The daemon answers each start with the authorization its method
// needs.
func newCodexSignIn(t *testing.T, clientID string) *codexSignIn {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.OpenAICodexClientID = clientID
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "fixture:codex-owner", "", "Codex Owner")
	if err != nil {
		t.Fatal(err)
	}
	signIn := &codexSignIn{owner: owner}
	daemon := http.NewServeMux()
	daemon.HandleFunc("POST /extension/v1/openai_codex/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		var start codexDaemonStart
		if err := json.NewDecoder(r.Body).Decode(&start); err != nil {
			t.Errorf("decode the start request: %v", err)
		}
		signIn.mu.Lock()
		signIn.starts = append(signIn.starts, start)
		// The gateway finds a flow by its state when the owner returns, so
		// it refuses a start whose state another flow already holds.
		attempt := strconv.Itoa(len(signIn.starts))
		signIn.mu.Unlock()
		authorization := oauthflow.Authorization{
			UserCode: "FIXTURE-CODE", VerificationURI: "https://auth.example.test/device",
			Secrets: oauthflow.Secrets{DeviceCode: "fixture-device-code-" + attempt},
		}
		if start.Method != string(oauthflow.MethodDevice) {
			authorization = oauthflow.Authorization{
				AuthorizationURL: "https://auth.example.test/authorize",
				Secrets:          oauthflow.Secrets{State: "fixture-state-" + attempt, Verifier: "fixture-verifier"},
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"authorization": authorization})
	})
	serveExtensionDaemon(t, daemon)
	signIn.server = httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(signIn.server.Close)
	return signIn
}

// codexStarter starts a Codex sign-in through one of the gateway's routes: an
// administrator's for the owner, or an SSO user's for themself.
type codexStarter struct {
	route string
	start func(t *testing.T, signIn *codexSignIn, body map[string]any) (int, map[string]any)
}

var (
	adminCodexStarter = codexStarter{"administrator", func(t *testing.T, signIn *codexSignIn, body map[string]any) (int, map[string]any) {
		return jsonRequest(t, signIn.server.URL+"/admin/api/principals/"+signIn.owner.ID+"/connections/codex/oauth/start",
			http.MethodPost, "admin-secret", body)
	}}
	selfServiceCodexStarter = codexStarter{"self-service", func(t *testing.T, signIn *codexSignIn, body map[string]any) (int, map[string]any) {
		return ssoConnectionRequest(t, signIn.server.URL, "codex-user", http.MethodPost,
			"/user/api/connections/codex/oauth/start", body)
	}}
	codexStarters = []codexStarter{adminCodexStarter, selfServiceCodexStarter}
)

// codexFlow is a sign-in flow the console offers for Codex, with the method
// the daemon runs it as and the flow the start answers.
type codexFlow struct{ flow, method, answer string }

var codexFlows = []codexFlow{
	{"device_code", string(oauthflow.MethodDevice), "device_code"},
	{"browser", string(oauthflow.MethodManual), codexBrowserManualProfile},
}

// startCodex starts a sign-in that must succeed, with body added to the
// request, and returns the one start the daemon received for it.
func (c *codexSignIn) startCodex(t *testing.T, starter codexStarter, flow codexFlow, body map[string]any) codexDaemonStart {
	t.Helper()
	request := map[string]any{"flow": flow.flow, "connection_name": "personal"}
	for key, value := range body {
		request[key] = value
	}
	c.mu.Lock()
	received := len(c.starts)
	c.mu.Unlock()
	status, response := starter.start(t, c, request)
	if status != http.StatusOK || response["flow"] != flow.answer {
		t.Fatalf("%s %s start: status=%d body=%+v", starter.route, flow.flow, status, response)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.starts) != received+1 {
		t.Fatalf("%s %s start reached the daemon %d times, want once", starter.route, flow.flow, len(c.starts)-received)
	}
	start := c.starts[received]
	if start.Method != flow.method {
		t.Fatalf("%s %s start ran as %q, want %q", starter.route, flow.flow, start.Method, flow.method)
	}
	return start
}

// Codex sign-in runs through the companion daemon, which signs in with its
// own OAuth client unless the gateway names one. Without a configured client
// every start reaches the daemon and names none, rather than an empty one.
func TestCodexSignInStartsWithoutAConfiguredClientID(t *testing.T) {
	signIn := newCodexSignIn(t, "")
	for _, starter := range codexStarters {
		for _, flow := range codexFlows {
			start := signIn.startCodex(t, starter, flow, nil)
			if clientID, named := start.Params["client_id"]; named {
				t.Errorf("%s %s start named client %q; with none configured it must name none", starter.route, flow.flow, clientID)
			}
		}
	}
}

// A configured Codex client ID reaches the daemon with every start, and so
// does one an administrator names at sign-in, which the gateway saves first.
func TestCodexSignInForwardsTheConfiguredClientID(t *testing.T) {
	signIn := newCodexSignIn(t, " fixture-codex-client ")
	for _, starter := range codexStarters {
		for _, flow := range codexFlows {
			start := signIn.startCodex(t, starter, flow, nil)
			if got := start.Params["client_id"]; got != "fixture-codex-client" {
				t.Errorf("%s %s start named client %q, want the configured one", starter.route, flow.flow, got)
			}
		}
	}
	for _, flow := range codexFlows {
		named := "fixture-admin-client-" + flow.flow
		start := signIn.startCodex(t, adminCodexStarter, flow, map[string]any{"client_id": named})
		if got := start.Params["client_id"]; got != named {
			t.Errorf("%s start named client %q, want the administrator's %q", flow.flow, got, named)
		}
		if got := config.Get().OpenAICodexClientID; got != named {
			t.Errorf("%s start left the configured client %q, want %q", flow.flow, got, named)
		}
	}
}

// Only an administrator changes the Codex client ID. A self-service start
// that names one signs in with the configured client and leaves the setting
// alone.
func TestOnlyAnAdministratorChangesTheCodexClientID(t *testing.T) {
	signIn := newCodexSignIn(t, "fixture-codex-client")
	for _, flow := range codexFlows {
		start := signIn.startCodex(t, selfServiceCodexStarter, flow, map[string]any{"client_id": "fixture-user-client"})
		if got := start.Params["client_id"]; got != "fixture-codex-client" {
			t.Errorf("self-service %s start named client %q, want the configured one", flow.flow, got)
		}
	}
	if err := configureOAuthClientID("codex", "fixture-user-client", false); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("a non-administrator configured the Codex client ID: err=%v", err)
	}
	if got := config.Get().OpenAICodexClientID; got != "fixture-codex-client" {
		t.Errorf("configured Codex client ID = %q, want it unchanged", got)
	}
}
