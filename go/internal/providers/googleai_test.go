package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// The response fixtures below are the real shapes captured from the live
// services during UAT, not invented ones.

// googleFixture configures instance "google" as cfg in a new installed
// Runtime, with the configured key its only credential, and returns the
// facade the provider factory builds for the gateway caller.
func googleFixture(t *testing.T, cfg *config.ProviderConfig) *googleProvider {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	oldKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{"google": cfg}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey, s.Providers = oldKey, oldProviders })
	})
	provider, err := runtime.instantiate(config.Get(), "google", cfg, gatewayCaller())
	if err != nil {
		t.Fatal(err)
	}
	google, ok := provider.(*googleProvider)
	if !ok {
		t.Fatalf("provider=%T, want the Google facade", provider)
	}
	return google
}

// studioFixture is googleFixture for AI Studio at base with the configured
// key, and vertexFixture for Vertex AI at base in project and location.
func studioFixture(t *testing.T, base, key string) *googleProvider {
	return googleFixture(t, &config.ProviderConfig{Type: "ai_studio", BaseURL: base, APIKey: key})
}

func vertexFixture(t *testing.T, base, key, project, location string) *googleProvider {
	return googleFixture(t, &config.ProviderConfig{
		Type: "vertex_ai", BaseURL: base, APIKey: key, Project: project, Location: location,
	})
}

// googleCatalog is a facade that serves only transport's catalog, as the
// facades the factory builds serve theirs.
func googleCatalog(transport GoogleAIProvider) Provider { return &googleProvider{legacy: transport} }

func TestModelURLDiffersPerSurface(t *testing.T) {
	studio := NewAIStudio("", "k", 0)
	got, err := studio.modelURL("models/gemini-3.5-flash", "generateContent")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.5-flash:generateContent" {
		t.Fatalf("ai_studio url = %q", got)
	}

	// The global endpoint has no region prefix in the host.
	global := NewVertexAI("", "k", "proj-1", "", 0)
	got, err = global.modelURL("gemini-3.5-flash", "generateContent")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://aiplatform.googleapis.com/v1/projects/proj-1/locations/global/publishers/google/models/gemini-3.5-flash:generateContent"
	if got != want {
		t.Fatalf("vertex global url = %q, want %q", got, want)
	}

	// A regional location prefixes the host and appears in the path.
	regional := NewVertexAI("", "k", "proj-1", "us-central1", 0)
	got, _ = regional.modelURL("veo-3.0-generate-001", "predictLongRunning")
	want = "https://us-central1-aiplatform.googleapis.com/v1/projects/proj-1/locations/us-central1/publishers/google/models/veo-3.0-generate-001:predictLongRunning"
	if got != want {
		t.Fatalf("vertex regional url = %q, want %q", got, want)
	}

	if _, err := NewVertexAI("", "k", "", "", 0).modelURL("m", "generateContent"); err == nil {
		t.Fatal("vertex without a project should refuse rather than build a broken url")
	}
}

// Google's refusals keep the message and status the transport gave them, and
// its routing: the status decides, and no Retry-After is passed on.
func TestUpstreamErrorsNameTheRealCause(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       string
		retryable  bool
	}{
		{
			name:      "billing exhausted",
			status:    429,
			body:      `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Your prepayment credits are depleted."}}`,
			want:      "ai_studio: provider billing exhausted — Your prepayment credits are depleted.",
			retryable: true,
		},
		{
			name:   "model not available",
			status: 404,
			body:   `{"error":{"code":404,"status":"NOT_FOUND","message":"Publisher model ... was not found or your project does not have access to it."}}`,
			want:   "ai_studio: model not available to this project or location — Publisher model ... was not found or your project does not have access to it.",
		},
		{
			name:   "credential rejected",
			status: 403,
			body:   `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied"}}`,
			want:   "ai_studio: credential rejected — denied",
		},
		{
			name:   "unclassified refusal",
			status: 400,
			body:   `{"error":{"code":400,"status":"INVALID_ARGUMENT","message":"bad request"}}`,
			want:   "ai_studio: bad request",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			provider := studioFixture(t, server.URL, "k")
			_, err := provider.Complete("gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, nil)
			if err == nil || err.Error() != testCase.want || UpstreamStatus(err) != testCase.status {
				t.Fatalf("error = %v (status %d), want %q with status %d", err, UpstreamStatus(err), testCase.want, testCase.status)
			}
			if InvocationRetryAfter(err) != "" || InvocationRetryable(err) != testCase.retryable ||
				InvocationFailoverEligible(err) != testCase.retryable || InvocationCircuitFailure(err) != testCase.retryable {
				t.Fatalf("status %d: retry-after=%q retryable=%v failover=%v circuit=%v", testCase.status, InvocationRetryAfter(err),
					InvocationRetryable(err), InvocationFailoverEligible(err), InvocationCircuitFailure(err))
			}
		})
	}
}

func TestCompleteTranslatesGeminiShape(t *testing.T) {
	var captured map[string]any
	var raw []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("api key must travel in the x-goog-api-key header, never the url")
		}
		if strings.Contains(r.URL.RawQuery, "key=") {
			t.Error("api key leaked into the query string")
		}
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		_, _ = w.Write([]byte(`{
          "candidates":[{"content":{"role":"model","parts":[{"text":"VERTEX OK"}]}}],
          "usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":95},
          "modelVersion":"gemini-3.5-flash"}`))
	}))
	defer server.Close()

	provider := studioFixture(t, server.URL, "secret")
	out, err := provider.Complete("gemini-3.5-flash", []Message{
		{"role": "system", "content": "be terse"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "hello"},
	}, Kwargs{"max_tokens": 16, "top_p": 0.9, "stream": false})
	if err != nil {
		t.Fatal(err)
	}
	// The body the transport encoded: only what it mapped reaches Google.
	if want := `{"contents":[{"parts":[{"text":"hi"}],"role":"user"},{"parts":[{"text":"hello"}],"role":"model"}],` +
		`"generationConfig":{"maxOutputTokens":16},"systemInstruction":{"parts":[{"text":"be terse"}]}}`; string(raw) != want {
		t.Fatalf("upstream body = %s\nwant %s", raw, want)
	}
	// System messages become systemInstruction; assistant becomes "model".
	if _, ok := captured["systemInstruction"]; !ok {
		t.Fatalf("system message was not mapped to systemInstruction: %+v", captured)
	}
	contents, _ := captured["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("contents = %d, want 2 (system is not a content)", len(contents))
	}
	second, _ := contents[1].(map[string]any)
	if second["role"] != "model" {
		t.Fatalf("assistant role = %v, want model", second["role"])
	}
	generation, _ := captured["generationConfig"].(map[string]any)
	if generation["maxOutputTokens"] == nil {
		t.Fatalf("max_tokens was not mapped to maxOutputTokens: %+v", generation)
	}

	choices, _ := out["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "VERTEX OK" || out["model"] != "gemini-3.5-flash" || out["id"] != "chatcmpl-google" {
		t.Fatalf("completion = %+v", out)
	}
	// The completion is decoded JSON now, so its numbers are float64.
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(7) || usage["total_tokens"] != float64(95) {
		t.Fatalf("usage = %+v", usage)
	}
}

func TestVertexRequestTypeHeaderIsExplicitAndInvocationOnly(t *testing.T) {
	for _, testCase := range []struct {
		name, mode, want string
	}{
		{name: "unset"},
		{name: "default", mode: "default"},
		{name: "paygo", mode: "paygo", want: "shared"},
		{name: "dedicated", mode: "dedicated", want: "dedicated"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("X-Vertex-AI-LLM-Request-Type")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
			}))
			defer server.Close()
			provider := googleFixture(t, &config.ProviderConfig{
				Type: "vertex_ai", BaseURL: server.URL, APIKey: "key", Project: "project", Location: "global", VertexRequestType: testCase.mode,
			})
			if _, err := provider.Complete("gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("request type header=%q, want %q", got, testCase.want)
			}
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Vertex-AI-LLM-Request-Type"); got != "" {
			t.Errorf("AI Studio received Vertex request header %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer server.Close()
	provider := googleFixture(t, &config.ProviderConfig{Type: "ai_studio", BaseURL: server.URL, APIKey: "key", VertexRequestType: "dedicated"})
	if _, err := provider.Complete("gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestGoogleEmbeddingTransportsNormalizeOpenAIEnvelope(t *testing.T) {
	t.Run("AI Studio", func(t *testing.T) {
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2,0.3]}}`))
		}))
		defer server.Close()
		result, err := studioFixture(t, server.URL, "key").Embed(context.Background(), "gemini-embedding-001", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if path != "/models/gemini-embedding-001:embedContent" || len(result["data"].([]any)[0].(map[string]any)["embedding"].([]any)) != 3 {
			t.Fatalf("path=%q result=%+v", path, result)
		}
	})

	t.Run("Vertex preserves input order", func(t *testing.T) {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"predictions":[{"embeddings":{"values":[%d,0.5],"statistics":{"token_count":%d}}}]}`, calls, calls+1)
		}))
		defer server.Close()
		result, err := vertexFixture(t, server.URL, "key", "project", "global").Embed(context.Background(), "text-embedding-005", []any{"first", "second"})
		if err != nil {
			t.Fatal(err)
		}
		data := result["data"].([]any)
		usage := result["usage"].(map[string]any)
		if calls != 2 || data[0].(map[string]any)["index"] != float64(0) || data[1].(map[string]any)["index"] != float64(1) ||
			data[1].(map[string]any)["embedding"].([]any)[0] != float64(2) || usage["prompt_tokens"] != float64(5) {
			t.Fatalf("calls=%d result=%+v", calls, result)
		}
	})

	t.Run("Vertex Gemini embedding 2 uses embedContent", func(t *testing.T) {
		var path string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embedding":{"values":[0.1,0.2]},"usageMetadata":{"promptTokenCount":3}}`))
		}))
		defer server.Close()
		result, err := vertexFixture(t, server.URL, "key", "project", "global").Embed(context.Background(), "gemini-embedding-2", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(path, "/gemini-embedding-2:embedContent") || result["usage"].(map[string]any)["prompt_tokens"] != float64(3) {
			t.Fatalf("path=%q result=%+v", path, result)
		}
	})
}

func TestGenerateImagesDecodesInlineData(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[
          {"text":"here you go"},
          {"inlineData":{"mimeType":"image/png","data":"` + base64.StdEncoding.EncodeToString(pngBytes) + `"}}
        ]}}],
        "usageMetadata":{"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":1120}]}}`))
	}))
	defer server.Close()

	provider := vertexFixture(t, server.URL, "k", "proj", "global")
	images, usage, err := provider.GenerateImages("gemini-3.1-flash-image", "an origami crane", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(images) != 1 || string(images[0].Data) != string(pngBytes) || images[0].MimeType != "image/png" {
		t.Fatalf("images = %+v", images)
	}
	// Image cost arrives as modality-tagged tokens, so accounting stays uniform.
	if usage["candidatesTokensDetails"] == nil {
		t.Fatalf("per-modality usage was dropped: %+v", usage)
	}
	generation, _ := captured["generationConfig"].(map[string]any)
	modalities, _ := generation["responseModalities"].([]any)
	if len(modalities) != 2 {
		t.Fatalf("responseModalities = %+v, want TEXT and IMAGE", modalities)
	}
}

func TestGenerateImagesRefusesTextOnlyModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"I cannot draw"}]}}]}`))
	}))
	defer server.Close()
	_, _, err := studioFixture(t, server.URL, "k").GenerateImages("gemini-3.5-flash", "a crane", 1)
	if err == nil || err.Error() != "ai_studio: the model returned no image data — it may be a text-only model" || UpstreamStatus(err) != 0 {
		t.Fatalf("err = %v, want a clear text-only-model message", err)
	}
}

func TestVideoStartAndPollAcrossBothResponseShapes(t *testing.T) {
	mp4 := []byte("FAKE-MP4")
	shapes := map[string]string{
		"vertex": `{"done":true,"response":{"videos":[{"bytesBase64Encoded":"` + base64.StdEncoding.EncodeToString(mp4) + `","mimeType":"video/mp4"}]}}`,
		"studio": `{"done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"https://example.invalid/v.mp4"}}]}}}`,
	}
	for name, done := range shapes {
		t.Run(name, func(t *testing.T) {
			var startBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.URL.Path, "predictLongRunning") {
					_ = json.NewDecoder(r.Body).Decode(&startBody)
					_, _ = w.Write([]byte(`{"name":"operations/abc123"}`))
					return
				}
				_, _ = w.Write([]byte(done))
			}))
			defer server.Close()

			provider := NewAIStudio(server.URL, "k", 5)
			job, err := provider.StartVideo("veo-3.0-generate-001", "a desk", map[string]any{"resolution": "720p"})
			if err != nil {
				t.Fatal(err)
			}
			if job.Operation != "models/veo-3.0-generate-001/operations/abc123" || job.Done {
				t.Fatalf("start job = %+v", job)
			}
			// sampleCount is defaulted so the request is valid without callers
			// knowing the vendor's required parameters.
			parameters, _ := startBody["parameters"].(map[string]any)
			if parameters["sampleCount"] == nil {
				t.Fatalf("sampleCount was not defaulted: %+v", parameters)
			}

			finished, err := provider.PollVideo(job.Operation)
			if err != nil {
				t.Fatal(err)
			}
			if !finished.Done {
				t.Fatal("poll did not report completion")
			}
			if name == "vertex" && string(finished.Data) != string(mp4) {
				t.Fatalf("inline video bytes = %q", finished.Data)
			}
			if name == "studio" && finished.VideoURI == "" {
				t.Fatalf("video uri was dropped: %+v", finished)
			}
		})
	}
}

func TestCapabilitiesComeFromGoogleMetadata(t *testing.T) {
	cases := []struct {
		id      string
		methods []string
		want    string
	}{
		{"veo-3.1-fast-generate-preview", []string{"predictLongRunning"}, "video"},
		{"gemini-3.1-flash-image", []string{"generateContent"}, "image"},
		{"gemini-3.5-flash", []string{"generateContent", "countTokens"}, "chat"},
		{"text-embedding-004", []string{"embedContent"}, "embedding"},
	}
	for _, testCase := range cases {
		capabilities, endpoints := googleCapabilities(testCase.id, testCase.methods)
		if _, ok := capabilities[testCase.want]; !ok {
			t.Fatalf("%s capabilities = %+v, want %s", testCase.id, capabilities, testCase.want)
		}
		if testCase.want != "embedding" && len(endpoints) == 0 {
			t.Fatalf("%s declared no endpoints", testCase.id)
		}
	}
}

func TestAIStudioCatalogSkipsModelsWithNoUsableCapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[
          {"name":"models/gemini-3.5-flash","displayName":"Gemini 3.5 Flash","supportedGenerationMethods":["generateContent"]},
          {"name":"models/veo-3.1-fast-generate-preview","supportedGenerationMethods":["predictLongRunning"]},
          {"name":"models/legacy-thing","supportedGenerationMethods":["generateMessage"]}
        ]}`))
	}))
	defer server.Close()
	models := NewAIStudio(server.URL, "k", 5).ListModels()
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2 (the unusable one is skipped)", len(models))
	}
}

func TestAIStudioCatalogReportsAuthenticationFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"status":"PERMISSION_DENIED","message":"denied"}}`))
	}))
	defer server.Close()
	_, _, err := NewAIStudio(server.URL, "bad-key", 5).ListModelsWithError()
	code, detail, status := CatalogFailure(err)
	if code != "catalog_authentication_failed" || status != http.StatusForbidden ||
		!strings.Contains(detail, "rejected") {
		t.Fatalf("catalog failure=(%q,%q,%d)", code, detail, status)
	}
}

func TestVertexDetailedCatalogRequiresProject(t *testing.T) {
	_, _, err := NewVertexAI("", "key", "", "", 5).ListModelsWithError()
	code, detail, status := CatalogFailure(err)
	if code != "catalog_configuration_incomplete" || status != 0 ||
		!strings.Contains(detail, "project") {
		t.Fatalf("catalog failure=(%q,%q,%d)", code, detail, status)
	}
}

// A 200 carrying an empty string is the worst failure mode: the caller sees
// success and no content. Gemini's thinking models hit this whenever
// max_tokens is small enough that reasoning consumes the whole budget.
func TestEmptyReplyExplainsItself(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"MAX_TOKENS"}],
          "usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":0,"totalTokenCount":36,"thoughtsTokenCount":29}}`))
	}))
	defer server.Close()
	_, err := vertexFixture(t, server.URL, "k", "p", "global").Complete(
		"gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, Kwargs{"max_tokens": 32})
	if err == nil {
		t.Fatal("an empty reply must not be reported as success")
	}
	want := "vertex_ai: the model spent its entire output budget on reasoning (29 thinking tokens) and returned no text — raise max_tokens or omit it"
	if err.Error() != want || UpstreamStatus(err) != 0 || !InvocationFailoverEligible(err) || InvocationRetryable(err) || InvocationCircuitFailure(err) {
		t.Fatalf("error %q, want %q, failing over without a retry", err, want)
	}
}

func TestGoogleUsageIncludesThinkingTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":` +
			`{"promptTokenCount":7,"candidatesTokenCount":3,"thoughtsTokenCount":29,"totalTokenCount":39}}`))
	}))
	defer server.Close()
	out, err := studioFixture(t, server.URL, "k").Complete("m", []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != float64(7) || usage["completion_tokens"] != float64(32) || usage["total_tokens"] != float64(39) {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestGoogleMapsDeveloperMessageToSystemInstruction(t *testing.T) {
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&payload)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
	}))
	defer server.Close()
	if _, err := studioFixture(t, server.URL, "k").Complete("m", []Message{
		{"role": "developer", "content": "developer policy"},
		{"role": "user", "content": "hello"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	system, _ := payload["systemInstruction"].(map[string]any)
	parts, _ := system["parts"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "developer policy" {
		t.Fatalf("systemInstruction=%+v", system)
	}
	contents, _ := payload["contents"].([]any)
	if len(contents) != 1 || contents[0].(map[string]any)["role"] != "user" {
		t.Fatalf("contents=%+v", contents)
	}
}

func TestEmptyReplyWithoutThinkingReportsFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"SAFETY"}]}`))
	}))
	defer server.Close()
	_, err := studioFixture(t, server.URL, "k").Complete("m", []Message{{"role": "user", "content": "hi"}}, nil)
	if err == nil || err.Error() != "ai_studio: the model returned no text (finish reason SAFETY)" {
		t.Fatalf("err = %v, want the finish reason surfaced", err)
	}
}

// Vertex returns Google's generic 404 HTML for GET on an operation resource;
// long-running predictions must be fetched with POST :fetchPredictOperation.
func TestVertexPollsWithFetchPredictOperation(t *testing.T) {
	operation := "projects/p/locations/us-central1/publishers/google/models/veo-3.1-lite-generate-001/operations/abc"
	mp4 := []byte("FAKE-MP4")
	var method, path string
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"done":true,"response":{"videos":[{"bytesBase64Encoded":"` +
			base64.StdEncoding.EncodeToString(mp4) + `","mimeType":"video/mp4"}]}}`))
	}))
	defer server.Close()

	job, err := NewVertexAI(server.URL, "k", "p", "us-central1", 5).PollVideo(operation)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost {
		t.Fatalf("poll method = %s, want POST", method)
	}
	if !strings.HasSuffix(path, ":fetchPredictOperation") {
		t.Fatalf("poll path = %s, want the fetchPredictOperation action", path)
	}
	// The model must be derived from the operation name, not guessed.
	if !strings.Contains(path, "veo-3.1-lite-generate-001") {
		t.Fatalf("model not derived from operation name: %s", path)
	}
	if body["operationName"] != operation {
		t.Fatalf("operationName = %v", body["operationName"])
	}
	if !job.Done || string(job.Data) != string(mp4) {
		t.Fatalf("job = %+v", job)
	}
}

func TestVertexOperationModelExtraction(t *testing.T) {
	got := vertexOperationModel("projects/p/locations/us-central1/publishers/google/models/veo-3.1-lite-generate-001/operations/abc")
	if got != "veo-3.1-lite-generate-001" {
		t.Fatalf("model = %q", got)
	}
	if vertexOperationModel("operations/abc") != "" {
		t.Fatal("a name without a model segment must yield empty, not a guess")
	}
}
