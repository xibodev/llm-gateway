package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// oauthTestClock is the clock a test moves past a flow's interval or expiry.
type oauthTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func newOAuthTestClock() *oauthTestClock { return &oauthTestClock{now: time.Now()} }

func (c *oauthTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *oauthTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fixtureCodeDriver stands in for a code flow no fixture endpoint reaches,
// such as a consumer_manual client, whose endpoints are the provider's own.
// Each start gets its own OAuth state; the first gets fixture-state.
type fixtureCodeDriver struct {
	start     func(oauthflow.StartRequest)
	exchange  func(oauthflow.Flow, string) (tokenstore.Record, error)
	starts    atomic.Int32
	exchanges atomic.Int32
}

func (d *fixtureCodeDriver) Start(_ context.Context, request oauthflow.StartRequest) (oauthflow.Authorization, error) {
	if d.start != nil {
		d.start(request)
	}
	state := "fixture-state"
	if started := d.starts.Add(1); started > 1 {
		state = fmt.Sprintf("fixture-state-%d", started)
	}
	return oauthflow.Authorization{
		AuthorizationURL: "https://accounts.example.test/authorize?state=" + state + "&code_challenge=fixture-challenge",
		ExpiresIn:        10 * time.Minute,
		Secrets:          oauthflow.Secrets{Verifier: "fixture-verifier", State: state, RedirectURI: request.RedirectURI},
	}, nil
}

func (d *fixtureCodeDriver) Exchange(_ context.Context, flow oauthflow.Flow, code string) (tokenstore.Record, error) {
	d.exchanges.Add(1)
	return d.exchange(flow, code)
}

// useOAuthDriver makes s run every flow of method with driver.
func useOAuthDriver(s *server, method oauthflow.Method, driver oauthflow.Driver) {
	builtIn := s.oauthDrivers
	s.oauthDrivers = func(instance string, candidate oauthflow.Method) (oauthflow.Driver, error) {
		if candidate == method {
			return driver, nil
		}
		return builtIn(instance, candidate)
	}
}

// fixtureManualRecord is what the consumer_manual driver reports for a
// sign-in whose token names no lifetime, which the gateway stores as 0.
func fixtureManualRecord(access, refresh, project string) tokenstore.Record {
	return tokenstore.Record{
		AccessToken: access, RefreshToken: refresh, Expiry: time.Unix(0, 0),
		Metadata: map[string]string{
			providers.OAuthMetadataProjectID: project, providers.OAuthMetadataProfile: antigravityManualProfile,
			providers.OAuthMetadataClientID: "fixture-client",
		},
	}
}

// fixtureCopilot serves GitHub's device endpoints: a new device code for
// each start, and token answers from answer, which counts the polls.
func fixtureCopilot(t *testing.T, answer func(poll int32) string) *atomic.Int32 {
	t.Helper()
	var starts, polls atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_, _ = fmt.Fprintf(w, `{"device_code":"fixture-device-%d","user_code":"FIX-%d","verification_uri":"https://verify.example.test","interval":1,"expires_in":60}`, starts.Add(1), starts.Load())
			return
		}
		_, _ = w.Write([]byte(answer(polls.Add(1))))
	}))
	t.Cleanup(mock.Close)
	t.Cleanup(providers.SetCopilotEndpointsForTests(copilotauth.Endpoints{DeviceCodeURL: mock.URL + "/device", AccessTokenURL: mock.URL + "/token"}))
	return &polls
}

func pendingCopilot(int32) string { return `{"error":"authorization_pending"}` }

// useOAuthProviders configures the providers the flow tests sign in to, so a
// start never has to persist one.
func useOAuthProviders(t *testing.T, update func(*config.Settings)) {
	t.Helper()
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"copilot":            {Type: "github_copilot", RegistryID: "github_copilot"},
			"google-antigravity": {Type: "google_antigravity", RegistryID: "google_antigravity"},
		}
		settings.GoogleAntigravityClientID, settings.GoogleAntigravityClientSecret = "fixture-client", "fixture-secret"
		settings.OAuthPublicBaseURL = ""
		if update != nil {
			update(settings)
		}
	})
}

func startDevice(t *testing.T, s *server, principal iam.Principal) string {
	t.Helper()
	started, err := s.startOAuthFlow(principal, "copilot", "", false,
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9791/start", nil), "", iam.ConnectionSourceUser, "")
	if err != nil {
		t.Fatal(err)
	}
	return stringValueForTest(started["device_code"])
}

func pollDevice(s *server, principal iam.Principal, deviceCode string) map[string]any {
	return s.pollOAuthFlow(principal, "copilot", deviceCode, "personal", iam.ConnectionSourceUser)
}

// startBrowser starts an Antigravity sign-in through the gateway's callback
// and returns its flow ID and OAuth state.
func startBrowser(t *testing.T, s *server, principal iam.Principal) (string, string) {
	t.Helper()
	started, err := s.startOAuthFlow(principal, "google_antigravity", "", false,
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9791/start", nil), "personal", iam.ConnectionSourceUser, "")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(stringValueForTest(started["authorization_url"]))
	if err != nil {
		t.Fatal(err)
	}
	return stringValueForTest(started["flow_id"]), authorizationURL.Query().Get("state")
}

// A key hook stores the connection, so the Service's save only confirms it:
// it accepts the key of the connection its completion stored, and nothing
// else.
func TestOAuthConnectionStoreOnlyConfirmsTheStoredConnection(t *testing.T) {
	record := tokenstore.Record{AccessToken: "fixture-access"}
	completion := &oauthCompletion{connection: &iam.ProviderConnection{ID: "conn_fixture"}}
	ctx := withOAuthCompletion(context.Background(), completion)
	if saved, err := (oauthConnectionStore{}).Save(ctx, "conn_fixture", record); err != nil || saved.AccessToken != "fixture-access" {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	for name, ctx := range map[string]context.Context{
		"another key":     ctx,
		"no completion":   context.Background(),
		"none stored yet": withOAuthCompletion(context.Background(), &oauthCompletion{}),
	} {
		key := "conn_fixture"
		if name == "another key" {
			key = "conn_other"
		}
		if _, err := (oauthConnectionStore{}).Save(ctx, key, record); err == nil {
			t.Fatalf("%s: save was accepted", name)
		}
	}
}

// A principal keeps at most maxOAuthFlowsPerPrincipal pending flows, of any
// method; one more evicts the oldest, and other principals are unaffected.
func TestOAuthFlowsCapOutstandingFlowsPerPrincipal(t *testing.T) {
	useOAuthProviders(t, nil)
	fixtureCopilot(t, pendingCopilot)
	clock := newOAuthTestClock()
	s := newServer(Runtime{}, clock.Now)
	principal, other := iam.Principal{ID: "principal", Kind: "human"}, iam.Principal{ID: "other", Kind: "human"}
	otherFlow := startDevice(t, s, other)
	browserFlow, _ := startBrowser(t, s, principal)
	flows := []string{}
	for index := 0; index < maxOAuthFlowsPerPrincipal; index++ {
		clock.Advance(time.Second)
		flows = append(flows, startDevice(t, s, principal))
	}
	clock.Advance(time.Second)
	if response := s.pollOAuthFlow(principal, "google_antigravity", browserFlow, "", iam.ConnectionSourceUser); response["status"] != "expired" {
		t.Fatalf("oldest flow was not evicted: %+v", response)
	}
	for _, flow := range append(flows, otherFlow) {
		owner := principal
		if flow == otherFlow {
			owner = other
		}
		if response := pollDevice(s, owner, flow); response["status"] != "pending" {
			t.Fatalf("flow %q response=%+v", flow, response)
		}
	}
}

// A poll the provider answers after the cap evicted its flow cannot store
// the flow again, or anything from it.
func TestOAuthPollCannotResurrectEvictedFlow(t *testing.T) {
	useOAuthProviders(t, nil)
	polled, release := make(chan struct{}), make(chan struct{})
	polls := fixtureCopilot(t, func(poll int32) string {
		if poll == 1 {
			close(polled)
			<-release
		}
		return `{"access_token":"fixture-access"}`
	})
	clock := newOAuthTestClock()
	s := newServer(Runtime{}, clock.Now)
	principal := iam.Principal{ID: "principal", Kind: "human"}
	evicted := startDevice(t, s, principal)
	clock.Advance(time.Second)
	answered := make(chan map[string]any)
	go func() { answered <- pollDevice(s, principal, evicted) }()
	<-polled
	for index := 0; index < maxOAuthFlowsPerPrincipal; index++ {
		clock.Advance(time.Second)
		startDevice(t, s, principal)
	}
	close(release)
	if response := <-answered; response["status"] != "expired" || response["connection"] != nil {
		t.Fatalf("evicted flow's approval response=%+v", response)
	}
	clock.Advance(time.Minute)
	if response := pollDevice(s, principal, evicted); response["status"] != "expired" || polls.Load() != 1 {
		t.Fatalf("evicted flow was polled again: response=%+v polls=%d", response, polls.Load())
	}
}

// A redirect that arrives at another provider's callback spends nothing: the
// flow stays pending for the redirect to its own.
func TestBrowserOAuthWrongCallbackPathDoesNotConsumeFlow(t *testing.T) {
	useOAuthProviders(t, nil)
	s := newServer(Runtime{}, time.Now)
	principal := iam.Principal{ID: "owner", Kind: "human"}
	flowID, state := startBrowser(t, s, principal)
	request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9791/oauth/callback/wrong?code=fixture-code&state="+url.QueryEscape(state), nil)
	request.SetPathValue("provider_id", "wrong")
	recorder := httptest.NewRecorder()
	s.handleOAuthBrowserCallback(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}
	if response := s.pollOAuthFlow(principal, "google_antigravity", flowID, "", iam.ConnectionSourceUser); response["status"] != "pending" {
		t.Fatalf("wrong callback path consumed browser OAuth flow: %+v", response)
	}
}
