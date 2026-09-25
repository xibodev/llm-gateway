package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/xibodev/llmgw-core"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestGatewayProviderOrchestratorKeepsBespokeTransportsInApplication(t *testing.T) {
	for _, registryID := range []string{"opencode_zen", "pollinations"} {
		if !usesApplicationAnonymousAdapter(registryID) {
			t.Fatalf("%s did not retain its application adapter", registryID)
		}
	}
	for _, registryID := range []string{"llm7", "kilo_code", "ovh_ai_endpoints"} {
		if usesApplicationAnonymousAdapter(registryID) {
			t.Fatalf("%s did not use the core anonymous adapter", registryID)
		}
	}
}

// installAnonymousFixture configures instances on a Runtime of the test's own,
// over a fresh state directory.
func installAnonymousFixture(t *testing.T, instances map[string]*config.ProviderConfig) *Runtime {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.Providers = instances
	})
	t.Cleanup(func() {
		iam.ResetForTests()
		config.Update(func(s *config.Settings) { *s = old })
	})
	return InstallForTests(t)
}

// anonymousProbeRequest is a probe of model as the anonymous orchestrator
// encodes it.
func anonymousProbeRequest(model string) core.Request {
	body, _ := json.Marshal(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": "Reply with: ok"}},
		"max_tokens": 16, "model": model, "stream": false,
	})
	return core.Request{Surface: core.ModelSurfaceChatCompletions, Model: model, Body: body, ContentType: core.ContentTypeJSON}
}

func TestAnonymousCatalogRefreshesAndKeepsTheFailureStatus(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusOK)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if code := int(status.Load()); code != http.StatusOK {
			http.Error(w, "refused", code)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"id":"codestral-latest","tier":"turbo","usage_based_only":false,"model_type":"chat","schema_endpoints":["openai"]},
			{"id":"paid","tier":"pro","usage_based_only":true,"model_type":"chat","schema_endpoints":["openai"]}]}`))
	}))
	defer upstream.Close()
	rt := installAnonymousFixture(t, map[string]*config.ProviderConfig{
		"catalog-fixture": {Type: "openai_compatible", RegistryID: "llm7", BaseURL: upstream.URL},
	})
	caller := core.Caller{Kind: core.CallerAnonymous}
	models, err := rt.AnonymousCatalog().Discover(context.Background(), caller, "catalog-fixture")
	if err != nil || len(models) != 1 || models[0].ID != "codestral-latest" || models[0].Object != "model" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	status.Store(http.StatusUnauthorized)
	models, err = rt.AnonymousCatalog().Discover(context.Background(), caller, "catalog-fixture")
	if models != nil || err == nil || err.Error() != "provider catalog failed" ||
		core.ClassifyError(err).StatusCode != http.StatusUnauthorized {
		t.Fatalf("a refused catalog: models=%+v err=%v", models, err)
	}
}

// probeRecorder answers probes, refusing a model named by refusals with its
// status and a Retry-After, and keeps each probe.
type probeRecorder struct {
	mu       sync.Mutex
	refusals map[string]int
	probes   []recordedProbe
}

type recordedProbe struct {
	path, authorization string
	body                map[string]any
}

func (p *probeRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.mu.Lock()
	p.probes = append(p.probes, recordedProbe{r.URL.Path, r.Header.Get("Authorization"), body})
	p.mu.Unlock()
	model, _ := body["model"].(string)
	if status := p.refusals[model]; status != 0 {
		w.Header().Set("Retry-After", "17")
		http.Error(w, "refused", status)
		return
	}
	_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
}

func (p *probeRecorder) last() recordedProbe {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.probes) == 0 {
		return recordedProbe{}
	}
	return p.probes[len(p.probes)-1]
}

// A keyless profile is probed as the gateway always probed it: the connector's
// payload, posted without a credential to the base URL's /chat/completions,
// and a refusal keeps its status and Retry-After.
func TestAnonymousInvokerProbesKeylessProfilesWithTheCoreClient(t *testing.T) {
	recorder := &probeRecorder{refusals: map[string]int{"limited": http.StatusTooManyRequests}}
	upstream := httptest.NewServer(recorder)
	defer upstream.Close()
	profile := AnonymousProviderProfile{
		RegistryID: "llm7", ProviderID: "keyless-fixture", RuntimeType: "openai_compatible", BaseURL: upstream.URL,
	}
	invoker := NewRuntime().AnonymousInvoker([]AnonymousProviderProfile{profile})
	caller := core.Caller{Kind: core.CallerAnonymous}
	response, err := invoker.Invoke(context.Background(), caller, "keyless-fixture", anonymousProbeRequest("codestral-latest"))
	var answer map[string]any
	probe := recorder.last()
	if err != nil || json.Unmarshal(response.Body, &answer) != nil || answer["choices"] == nil ||
		probe.path != "/chat/completions" || probe.authorization != "" || len(probe.body) != 4 ||
		probe.body["max_tokens"] != float64(16) || probe.body["model"] != "codestral-latest" || probe.body["stream"] != false {
		t.Fatalf("response=%s err=%v probe=%+v", response.Body, err, probe)
	}
	_, err = invoker.Invoke(context.Background(), caller, "keyless-fixture", anonymousProbeRequest("limited"))
	if classification := core.ClassifyError(err); classification.StatusCode != http.StatusTooManyRequests ||
		classification.RetryAfter != 17*time.Second {
		t.Fatalf("err=%v classification=%+v", err, classification)
	}
}

// Pollinations is probed through its instance's provider, whose transport
// serves its /v1/chat/completions, and a refusal keeps its status.
func TestAnonymousInvokerProbesPollinationsThroughItsProvider(t *testing.T) {
	recorder := &probeRecorder{refusals: map[string]int{"refused": http.StatusUnauthorized}}
	upstream := httptest.NewServer(recorder)
	defer upstream.Close()
	rt := installAnonymousFixture(t, map[string]*config.ProviderConfig{
		"pollinations-fixture": {Type: "openai_compatible", RegistryID: "pollinations", BaseURL: upstream.URL},
	})
	profile := AnonymousProviderProfile{
		RegistryID: "pollinations", ProviderID: "pollinations-fixture", RuntimeType: "openai_compatible", BaseURL: upstream.URL,
	}
	invoker := rt.AnonymousInvoker([]AnonymousProviderProfile{profile})
	caller := core.Caller{Kind: core.CallerAnonymous}
	if _, err := invoker.Invoke(context.Background(), caller, "pollinations-fixture", anonymousProbeRequest("openai-fast")); err != nil {
		t.Fatal(err)
	}
	if probe := recorder.last(); probe.path != "/v1/chat/completions" ||
		probe.body["max_tokens"] != float64(16) || probe.body["model"] != "openai-fast" {
		t.Fatalf("probe=%+v", probe)
	}
	_, err := invoker.Invoke(context.Background(), caller, "pollinations-fixture", anonymousProbeRequest("refused"))
	if classification := core.ClassifyError(err); classification.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err=%v classification=%+v", err, classification)
	}
}
