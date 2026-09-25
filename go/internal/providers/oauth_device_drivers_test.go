package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// requestLog records what a fixture provider was asked, so a driver can be
// shown to send exactly what the adapter it replaces sends.
type requestLog struct {
	mu       sync.Mutex
	requests []string
}

func (l *requestLog) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests = append(l.requests, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type")+" "+string(body))
}

func (l *requestLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	taken := l.requests
	l.requests = nil
	return taken
}

func sameRequests(t *testing.T, what string, adapter, driver []string) {
	t.Helper()
	if strings.Join(adapter, "\n") != strings.Join(driver, "\n") {
		t.Fatalf("%s: driver requests differ\nadapter:\n%s\ndriver:\n%s", what, strings.Join(adapter, "\n"), strings.Join(driver, "\n"))
	}
}

// sameRecord checks that a driver's record carries what the adapter's
// result did, the expiry to the second.
func sameRecord(t *testing.T, record tokenstore.Record, result ProviderAuthPoll) {
	t.Helper()
	if record.AccessToken != result.AccessToken || record.RefreshToken != result.RefreshToken ||
		record.IDToken != result.IDToken || record.TokenType != result.TokenType ||
		record.Expiry.Unix() != result.ExpiresAt || record.AccountID != result.AccountID ||
		record.Metadata[OAuthMetadataAccountLabel] != result.AccountLabel ||
		record.Metadata[OAuthMetadataProjectID] != result.ProjectID ||
		record.Metadata[OAuthMetadataProfile] != result.OAuthProfile ||
		record.Metadata[OAuthMetadataClientID] != result.OAuthClientID {
		t.Fatalf("record=%+v metadata=%v, adapter result=%+v", record, record.Metadata, result)
	}
}

// samePoll checks that a driver's poll ended as the adapter's did, and
// noted the provider's answer as the adapter reported it.
func samePoll(t *testing.T, what string, result oauthflow.PollResult, err error, note *OAuthPollNote, want oauthflow.PollStatus, polled ProviderAuthPoll) {
	t.Helper()
	if err != nil || result.Status != want || !note.Polled || note.Status != polled.Status || note.Detail != polled.Error {
		t.Fatalf("%s: result=%v err=%v note=%+v adapter=%+v", what, result.Status, err, note, polled)
	}
	if want == oauthflow.PollApproved {
		sameRecord(t, result.Record, polled)
	}
}

func oauthDriver(t *testing.T, adapterID string, method oauthflow.Method) oauthflow.Driver {
	t.Helper()
	driver, err := Current().OAuthDriver(adapterID, "fixture-provider", method)
	if err != nil {
		t.Fatal(err)
	}
	return driver
}

func TestOAuthDriverOffersOnlyTheConsoleFlows(t *testing.T) {
	for _, unsupported := range []struct {
		adapter string
		method  oauthflow.Method
	}{
		{"github_copilot", oauthflow.MethodBrowser}, {"openai_codex", oauthflow.MethodBrowser},
		{"google_antigravity", oauthflow.MethodDevice}, {"fixture_auth", oauthflow.MethodDevice},
	} {
		if _, err := Current().OAuthDriver(unsupported.adapter, "fixture-provider", unsupported.method); err == nil {
			t.Fatalf("%s offered %s", unsupported.adapter, unsupported.method)
		}
	}
}

func TestCopilotDeviceDriverSendsWhatTheAdapterSends(t *testing.T) {
	log := &requestLog{}
	token := `{"error":"authorization_pending"}`
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_, _ = w.Write([]byte(`{"device_code":"fixture-device","user_code":"FIX-1234","verification_uri":"https://verify.example.test","interval":3,"expires_in":90}`))
			return
		}
		_, _ = w.Write([]byte(token))
	}))
	defer mock.Close()
	t.Cleanup(SetCopilotEndpointsForTests(copilotauth.Endpoints{DeviceCodeURL: mock.URL + "/device", AccessTokenURL: mock.URL + "/token"}))
	ctx := context.Background()
	adapter := githubCopilotAuthAdapter{}
	driver := oauthDriver(t, "github_copilot", oauthflow.MethodDevice).(oauthflow.DeviceDriver)

	start, err := adapter.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adapterRequests := log.take()
	authorization, err := driver.Start(ctx, oauthflow.StartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sameRequests(t, "start", adapterRequests, log.take())
	if authorization.Secrets.DeviceCode != start.DeviceCode || authorization.UserCode != start.UserCode ||
		authorization.VerificationURI != start.VerificationURI || authorization.Interval != 3*time.Second ||
		authorization.ExpiresIn != 90*time.Second {
		t.Fatalf("authorization=%+v start=%+v", authorization, start)
	}

	flow := oauthflow.Flow{Secrets: authorization.Secrets}
	for _, check := range []struct {
		response string
		want     oauthflow.PollStatus
	}{
		{`{"error":"authorization_pending"}`, oauthflow.PollPending},
		{`{"error":"slow_down"}`, oauthflow.PollSlowDown},
		{`{"error":"access_denied"}`, oauthflow.PollDenied},
		{`{"access_token":"fixture-access"}`, oauthflow.PollApproved},
	} {
		token = check.response
		polled := adapter.PollDevice(ctx, start.DeviceCode, start.PrivateState)
		adapterRequests := log.take()
		noted, note := WithOAuthPollNote(ctx)
		result, err := driver.Poll(noted, flow)
		sameRequests(t, "poll", adapterRequests, log.take())
		samePoll(t, check.response, result, err, note, check.want, polled)
	}
}

func TestCodexDeviceDriverSendsWhatTheAdapterSends(t *testing.T) {
	log := &requestLog{}
	deviceToken := `{"authorization_code":"fixture-code","code_verifier":"fixture-verifier"}`
	status := http.StatusOK
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			_, _ = w.Write([]byte(`{"device_auth_id":"fixture-device","user_code":"CODEX-1","interval":"2","expires_in":120}`))
		case "/device-token":
			w.WriteHeader(status)
			_, _ = w.Write([]byte(deviceToken))
		default:
			_, _ = w.Write([]byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","id_token":"fixture-id","token_type":"Bearer","expires_at":4102444800,"account_id":"fixture-account","account_label":"Fixture"}`))
		}
	}))
	defer mock.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{
		UserCodeURL: mock.URL + "/usercode", DeviceTokenURL: mock.URL + "/device-token", OAuthTokenURL: mock.URL + "/token",
	}})
	ctx := context.Background()
	adapter := openAICodexAuthAdapter{clientID: EffectiveCodexClientID()}
	driver := oauthDriver(t, "openai_codex", oauthflow.MethodDevice).(oauthflow.DeviceDriver)

	start, err := adapter.StartDevice(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adapterRequests := log.take()
	authorization, err := driver.Start(ctx, oauthflow.StartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	sameRequests(t, "start", adapterRequests, log.take())
	if authorization.Secrets.DeviceCode != start.DeviceCode || authorization.UserCode != start.UserCode ||
		authorization.VerificationURI != start.VerificationURI || authorization.Interval != 2*time.Second ||
		authorization.Secrets.DriverData["client_id"] != adapter.clientID {
		t.Fatalf("authorization=%+v start=%+v", authorization, start)
	}

	flow := oauthflow.Flow{Secrets: authorization.Secrets}
	for _, check := range []struct {
		status   int
		response string
		want     oauthflow.PollStatus
	}{
		{http.StatusBadRequest, `{"error":"authorization_pending"}`, oauthflow.PollPending},
		{http.StatusBadRequest, `{"error":"slow_down"}`, oauthflow.PollSlowDown},
		{http.StatusBadRequest, `{"error":"expired_token"}`, oauthflow.PollExpired},
		{http.StatusBadRequest, `{"error":"access_denied"}`, oauthflow.PollDenied},
		{http.StatusOK, `{"authorization_code":"fixture-code","code_verifier":"fixture-verifier"}`, oauthflow.PollApproved},
	} {
		status, deviceToken = check.status, check.response
		polled := adapter.PollDevice(ctx, start.DeviceCode, start.PrivateState)
		adapterRequests := log.take()
		noted, note := WithOAuthPollNote(ctx)
		result, err := driver.Poll(noted, flow)
		sameRequests(t, "poll", adapterRequests, log.take())
		samePoll(t, check.response, result, err, note, check.want, polled)
	}
}

// A poll that fails in transit keeps the flow pending: the note carries the
// provider's status, and the error its explanation.
func TestDevicePollErrorsAreTransient(t *testing.T) {
	noted, note := WithOAuthPollNote(context.Background())
	_, err := devicePollResult(noted, "error", "transport error: Bearer fixture-secret-token", tokenstore.Record{})
	if err == nil || strings.Contains(err.Error(), "fixture-secret-token") || note.Status != "error" || note.Detail != err.Error() {
		t.Fatalf("err=%v note=%+v", err, note)
	}
	if _, err := devicePollResult(context.Background(), "unexpected", "", tokenstore.Record{}); err == nil {
		t.Fatal("an unknown status was not transient")
	}
}
