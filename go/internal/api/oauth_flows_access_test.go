package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// useOAuthIAM opens a fresh gateway database that encrypts credentials and
// trusts the fixture SSO proxy, and returns a human principal to sign in.
func useOAuthIAM(t *testing.T, update func(*config.Settings)) iam.Principal {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	useOAuthProviders(t, func(settings *config.Settings) {
		settings.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		settings.APIKey = "admin-secret"
		settings.SSOEnabled, settings.SSOSharedSecret, settings.SSOAutoProvision = true, "proxy-secret", true
		settings.GoogleAntigravityOAuthProfile = antigravityManualProfile
		settings.GoogleAntigravityClientMode = "public"
		settings.GoogleAntigravityRedirectURI = "https://callback.example.test/oauth"
		if update != nil {
			update(settings)
		}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("human", "fixture:oauth-owner", "", "OAuth Owner")
	if err != nil {
		t.Fatal(err)
	}
	return principal
}

func fixtureManualDriver() *fixtureCodeDriver {
	return &fixtureCodeDriver{exchange: func(oauthflow.Flow, string) (tokenstore.Record, error) {
		return fixtureManualRecord("fixture-access", "fixture-refresh", "fixture-project"), nil
	}}
}

func startManual(t *testing.T, s *server, principal iam.Principal) string {
	t.Helper()
	started, err := s.startManualOAuthFlow(principal, "google_antigravity", "personal", iam.ConnectionSourceUser, providers.ProviderAuthManualConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	return stringValueForTest(started["flow_id"])
}

// A pasted code completes its flow once: the same paste again, which a
// double submit sends, finds nothing to complete and exchanges nothing.
func TestOAuthManualCompletionCannotBeReplayed(t *testing.T) {
	principal := useOAuthIAM(t, nil)
	s := newServer(Runtime{}, time.Now)
	driver := fixtureManualDriver()
	useOAuthDriver(s, oauthflow.MethodManual, driver)
	flowID := startManual(t, s, principal)
	if first := s.completeManualOAuthFlow(principal, "google_antigravity", flowID, "fixture-code"); first["status"] != "authorized" {
		t.Fatalf("first completion=%+v", first)
	}
	second := s.completeManualOAuthFlow(principal, "google_antigravity", flowID, "fixture-code")
	if second["status"] != "expired" || second["error"] != "Manual authorization is no longer active. Start again." || driver.exchanges.Load() != 1 {
		t.Fatalf("replayed completion=%+v exchanges=%d", second, driver.exchanges.Load())
	}
}

// Flows belong to the principal they were started for, an administrator's
// start included. Another principal's poll and completion answer that the
// flow is not active, with 200 as before, and leave it untouched.
func TestOAuthFlowsAnswerOnlyTheirPrincipal(t *testing.T) {
	owner := useOAuthIAM(t, nil)
	polls := fixtureCopilot(t, pendingCopilot)
	clock := newOAuthTestClock()
	s := newServer(Runtime{}, clock.Now)
	driver := fixtureManualDriver()
	useOAuthDriver(s, oauthflow.MethodManual, driver)
	gateway := httptest.NewServer(s.handler())
	defer gateway.Close()
	admin := func(path string, body map[string]any) (int, map[string]any) {
		return jsonRequest(t, gateway.URL+"/admin/api/principals/"+owner.ID+"/connections/"+path, http.MethodPost, "admin-secret", body)
	}
	intruder := func(path string, body map[string]any) (int, map[string]any) {
		return ssoConnectionRequest(t, gateway.URL, "fixture:intruder", http.MethodPost, "/user/api/connections/"+path, body)
	}
	_, device := admin("copilot/oauth/start", map[string]any{})
	_, manual := admin("google_antigravity/oauth/start", map[string]any{"profile": antigravityManualProfile})
	deviceCode, flowID := stringValueForTest(device["device_code"]), stringValueForTest(manual["flow_id"])
	if deviceCode == "" || flowID == "" {
		t.Fatalf("device=%+v manual=%+v", device, manual)
	}
	clock.Advance(time.Second)

	status, polled := intruder("copilot/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || polled["status"] != "expired" || polls.Load() != 0 {
		t.Fatalf("intruder poll status=%d payload=%+v provider polls=%d", status, polled, polls.Load())
	}
	status, completed := intruder("google_antigravity/oauth/complete", map[string]any{"flow_id": flowID, "authorization_response": "fixture-code"})
	if status != http.StatusOK || completed["status"] != "expired" || driver.exchanges.Load() != 0 {
		t.Fatalf("intruder completion status=%d payload=%+v exchanges=%d", status, completed, driver.exchanges.Load())
	}

	if status, polled = admin("copilot/oauth/poll", map[string]any{"device_code": deviceCode}); status != http.StatusOK || polled["status"] != "pending" || polls.Load() != 1 {
		t.Fatalf("owner poll status=%d payload=%+v provider polls=%d", status, polled, polls.Load())
	}
	status, completed = admin("google_antigravity/oauth/complete", map[string]any{"flow_id": flowID, "authorization_response": "fixture-code"})
	connection, _ := completed["connection"].(map[string]any)
	if status != http.StatusOK || completed["status"] != "authorized" || connection["principal_id"] != owner.ID || connection["source"] != iam.ConnectionSourceAdmin {
		t.Fatalf("owner completion status=%d payload=%+v", status, completed)
	}
}

// Every method's flow ends at its expiry: polls, pastes and redirects that
// come later spend nothing and reach no provider.
func TestOAuthFlowsExpire(t *testing.T) {
	principal := useOAuthIAM(t, nil)
	polls := fixtureCopilot(t, pendingCopilot)
	clock := newOAuthTestClock()
	s := newServer(Runtime{}, clock.Now)
	driver := fixtureManualDriver()
	useOAuthDriver(s, oauthflow.MethodManual, driver)
	deviceCode := startDevice(t, s, principal)
	manualFlow := startManual(t, s, principal)
	browserFlow, state := startBrowser(t, s, principal)
	clock.Advance(10*time.Minute + time.Second)

	if response := pollDevice(s, principal, deviceCode); response["status"] != "expired" || response["error"] != "Device authorization expired. Start again." || polls.Load() != 0 {
		t.Fatalf("expired device poll=%+v provider polls=%d", response, polls.Load())
	}
	if response := s.completeManualOAuthFlow(principal, "google_antigravity", manualFlow, "fixture-code"); response["status"] != "expired" || driver.exchanges.Load() != 0 {
		t.Fatalf("expired manual completion=%+v exchanges=%d", response, driver.exchanges.Load())
	}
	if response := s.pollOAuthFlow(principal, "google_antigravity", browserFlow, "", iam.ConnectionSourceUser); response["status"] != "expired" || response["error"] != "Browser authorization expired. Start again." {
		t.Fatalf("expired browser poll=%+v", response)
	}
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9791/oauth/callback/google_antigravity?code=fixture-code&state="+url.QueryEscape(state), nil)
	request.SetPathValue("provider_id", "google_antigravity")
	recorder := httptest.NewRecorder()
	s.handleOAuthBrowserCallback(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "no longer active") {
		t.Fatalf("expired callback status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}

// The redirect that completes a browser flow carries no session, so a poll
// hands the connection over, as before to exactly one poll, which records
// the connection in the audit log once.
func TestBrowserOAuthOutcomeIsHandedOverOnce(t *testing.T) {
	useOAuthIAM(t, nil)
	s := newServer(Runtime{}, time.Now)
	useOAuthDriver(s, oauthflow.MethodBrowser, &fixtureCodeDriver{exchange: func(flow oauthflow.Flow, code string) (tokenstore.Record, error) {
		if code != "fixture-code" || !strings.HasSuffix(flow.Secrets.RedirectURI, "/oauth/callback/google_antigravity") {
			t.Fatalf("exchange code=%q redirect=%q", code, flow.Secrets.RedirectURI)
		}
		return fixtureManualRecord("fixture-access", "", "fixture-project"), nil
	}})
	gateway := httptest.NewServer(s.handler())
	defer gateway.Close()
	user := func(path string, body map[string]any) (int, map[string]any) {
		return ssoConnectionRequest(t, gateway.URL, "fixture:browser-user", http.MethodPost, "/user/api/connections/google_antigravity/oauth/"+path, body)
	}
	status, started := user("start", map[string]any{"connection_name": "personal"})
	if status != http.StatusOK || started["flow"] != "browser" {
		t.Fatalf("start status=%d payload=%+v", status, started)
	}
	flowID := stringValueForTest(started["flow_id"])
	if _, pending := user("poll", map[string]any{"flow_id": flowID}); pending["status"] != "pending" {
		t.Fatalf("poll before the callback=%+v", pending)
	}
	authorizationURL, _ := url.Parse(stringValueForTest(started["authorization_url"]))
	callback, err := http.Get(gateway.URL + "/oauth/callback/google_antigravity?code=fixture-code&state=" + url.QueryEscape(authorizationURL.Query().Get("state")))
	if err != nil {
		t.Fatal(err)
	}
	_ = callback.Body.Close()
	if callback.StatusCode != http.StatusOK {
		t.Fatalf("callback status=%d", callback.StatusCode)
	}
	_, authorized := user("poll", map[string]any{"flow_id": flowID})
	connection, _ := authorized["connection"].(map[string]any)
	if authorized["status"] != "authorized" || connection["connection_name"] != "personal" || connection["oauth_project_id"] != "fixture-project" {
		t.Fatalf("first poll after the callback=%+v", authorized)
	}
	if _, again := user("poll", map[string]any{"flow_id": flowID}); again["status"] != "expired" {
		t.Fatalf("second poll after the callback=%+v", again)
	}
	events, err := iam.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	connects := 0
	for _, event := range events {
		if event.Action == "oauth_connection.connect" {
			connects++
		}
	}
	if connects != 1 {
		t.Fatalf("connect audit events=%d", connects)
	}
}

// A device poll that fails in transit keeps the flow pending, so the next
// poll asks the provider again; the gateway used to end the flow instead.
func TestDevicePollTransientErrorKeepsFlowPending(t *testing.T) {
	useOAuthProviders(t, nil)
	polls := fixtureCopilot(t, pendingCopilot)
	clock := newOAuthTestClock()
	s := newServer(Runtime{}, clock.Now)
	principal := iam.Principal{ID: "principal", Kind: "human"}
	deviceCode := startDevice(t, s, principal)
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	restore := providers.SetCopilotEndpointsForTests(copilotauth.Endpoints{AccessTokenURL: unreachable.URL})
	clock.Advance(time.Second)
	failed := pollDevice(s, principal, deviceCode)
	restore()
	if failed["status"] != "error" || !strings.Contains(stringValueForTest(failed["error"]), "transport error") {
		t.Fatalf("failed poll=%+v", failed)
	}
	clock.Advance(time.Second)
	if response := pollDevice(s, principal, deviceCode); response["status"] != "pending" || polls.Load() != 1 {
		t.Fatalf("poll after a transient error=%+v provider polls=%d", response, polls.Load())
	}
}
