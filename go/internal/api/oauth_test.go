package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// codexDaemonStart is a Codex sign-in start as the companion daemon receives
// it.
type codexDaemonStart struct {
	Method string            `json:"method"`
	Params map[string]string `json:"params"`
}

// codexSignIn is a gateway whose Codex provider signs in through a fake
// companion daemon, which records every start it receives and answers every
// device poll with answer.
type codexSignIn struct {
	server *httptest.Server
	owner  iam.Principal
	mu     sync.Mutex
	starts []codexDaemonStart
	answer oauthflow.PollResult
	polls  int
	// skew is how far the gateway's clock runs ahead of the real one, so a
	// test lets a device flow's polling interval pass without waiting.
	skew atomic.Int64
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
		// A completed sign-in stores its connection encrypted.
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
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
	daemon.HandleFunc("POST /extension/v1/openai_codex/oauth/poll", func(w http.ResponseWriter, r *http.Request) {
		signIn.mu.Lock()
		signIn.polls++
		answer := signIn.answer
		signIn.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"result": answer})
	})
	serveExtensionDaemon(t, daemon)
	signIn.server = httptest.NewServer(newServer(Runtime{}, func() time.Time {
		return time.Now().Add(time.Duration(signIn.skew.Load()))
	}).handler())
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

// startDeviceSignIn starts an administrator's Codex device sign-in for the
// owner and returns the device code its polls carry.
func (c *codexSignIn) startDeviceSignIn(t *testing.T) string {
	t.Helper()
	status, response := adminCodexStarter.start(t, c, map[string]any{"flow": "device_code", "connection_name": "personal"})
	deviceCode, _ := response["device_code"].(string)
	if status != http.StatusOK || deviceCode == "" {
		t.Fatalf("device start: status=%d body=%+v", status, response)
	}
	return deviceCode
}

// pollDeviceSignIn polls a device sign-in as the console does, with the
// daemon answering answer, and returns the gateway's answer. The gateway's
// clock first moves a minute on, past any polling interval the flow has.
func (c *codexSignIn) pollDeviceSignIn(t *testing.T, deviceCode string, answer oauthflow.PollResult) map[string]any {
	t.Helper()
	c.mu.Lock()
	c.answer = answer
	c.mu.Unlock()
	c.skew.Add(int64(time.Minute))
	status, response := jsonRequest(t, c.server.URL+"/admin/api/principals/"+c.owner.ID+"/connections/codex/oauth/poll",
		http.MethodPost, "admin-secret", map[string]any{"device_code": deviceCode, "connection_name": "personal"})
	if status != http.StatusOK {
		t.Fatalf("device poll: status=%d body=%+v", status, response)
	}
	return response
}

// daemonPolls is how many device polls reached the daemon.
func (c *codexSignIn) daemonPolls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.polls
}

// A device sign-in waits for its owner: every poll answers what the provider
// answered while it waits, so the console keeps polling, and the approval it
// reports at last stores the connection the poll names.
func TestDeviceSignInAnswersPendingUntilTheOwnerApproves(t *testing.T) {
	signIn := newCodexSignIn(t, "")
	deviceCode := signIn.startDeviceSignIn(t)
	for _, answer := range []oauthflow.PollStatus{oauthflow.PollPending, oauthflow.PollSlowDown, oauthflow.PollPending} {
		if got := signIn.pollDeviceSignIn(t, deviceCode, oauthflow.PollResult{Status: answer}); got["status"] != string(answer) {
			t.Fatalf("a poll the provider answered %s answered %+v", answer, got)
		}
	}
	got := signIn.pollDeviceSignIn(t, deviceCode, oauthflow.PollResult{Status: oauthflow.PollApproved, Record: tokenstore.Record{
		AccessToken: "fixture-access", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour),
	}})
	connection, _ := got["connection"].(map[string]any)
	if got["status"] != "authorized" || connection["connection_name"] != "personal" || connection["provider_id"] != "codex" {
		t.Fatalf("the approved poll answered %+v", got)
	}
	if polls := signIn.daemonPolls(); polls != 4 {
		t.Fatalf("the provider was polled %d times, want 4", polls)
	}
}

// A denial or the provider's expiry ends a device sign-in: the poll says
// which, and a later poll finds the flow over without asking the provider.
func TestDeviceSignInReportsDenialAndExpiry(t *testing.T) {
	for _, answer := range []oauthflow.PollStatus{oauthflow.PollDenied, oauthflow.PollExpired} {
		t.Run(string(answer), func(t *testing.T) {
			signIn := newCodexSignIn(t, "")
			deviceCode := signIn.startDeviceSignIn(t)
			got := signIn.pollDeviceSignIn(t, deviceCode, oauthflow.PollResult{Status: answer})
			if got["status"] != string(answer) || got["error"] == nil {
				t.Fatalf("a poll the provider answered %s answered %+v", answer, got)
			}
			later := signIn.pollDeviceSignIn(t, deviceCode, oauthflow.PollResult{Status: oauthflow.PollPending})
			if later["status"] != "expired" || signIn.daemonPolls() != 1 {
				t.Fatalf("a poll after the %s answered %+v; the provider was polled %d times, want once", answer, later, signIn.daemonPolls())
			}
		})
	}
}
