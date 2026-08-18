package providers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The response fixtures below are the real shapes captured from the live
// services during UAT, not invented ones.

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

func TestUpstreamErrorsNameTheRealCause(t *testing.T) {
	cases := []struct {
		name, body string
		status     int
		want       string
	}{
		{
			name:   "billing exhausted",
			status: 429,
			body:   `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Your prepayment credits are depleted."}}`,
			want:   "provider billing exhausted",
		},
		{
			name:   "model not available",
			status: 404,
			body:   `{"error":{"code":404,"status":"NOT_FOUND","message":"Publisher model ... was not found or your project does not have access to it."}}`,
			want:   "model not available to this project or location",
		},
		{
			name:   "credential rejected",
			status: 403,
			body:   `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied"}}`,
			want:   "credential rejected",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(testCase.status)
				_, _ = w.Write([]byte(testCase.body))
			}))
			defer server.Close()
			provider := NewAIStudio(server.URL, "k", 5)
			_, err := provider.Complete("gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, nil)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error = %v, want it to mention %q", err, testCase.want)
			}
		})
	}
}

func TestCompleteTranslatesGeminiShape(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-goog-api-key") == "" {
			t.Error("api key must travel in the x-goog-api-key header, never the url")
		}
		if strings.Contains(r.URL.RawQuery, "key=") {
			t.Error("api key leaked into the query string")
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_, _ = w.Write([]byte(`{
          "candidates":[{"content":{"role":"model","parts":[{"text":"VERTEX OK"}]}}],
          "usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":95},
          "modelVersion":"gemini-3.5-flash"}`))
	}))
	defer server.Close()

	provider := NewAIStudio(server.URL, "secret", 5)
	out, err := provider.Complete("gemini-3.5-flash", []Message{
		{"role": "system", "content": "be terse"},
		{"role": "user", "content": "hi"},
		{"role": "assistant", "content": "hello"},
	}, Kwargs{"max_tokens": 16})
	if err != nil {
		t.Fatal(err)
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
	if message["content"] != "VERTEX OK" {
		t.Fatalf("content = %v", message["content"])
	}
	usage, _ := out["usage"].(map[string]any)
	if usage["prompt_tokens"] != 7 || usage["total_tokens"] != 95 {
		t.Fatalf("usage = %+v", usage)
	}
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

	provider := NewVertexAI(server.URL, "k", "proj", "global", 5)
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
	_, _, err := NewAIStudio(server.URL, "k", 5).GenerateImages("gemini-3.5-flash", "a crane", 1)
	if err == nil || !strings.Contains(err.Error(), "no image data") {
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
			if job.Operation != "operations/abc123" || job.Done {
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

func TestVertexCatalogIsCuratedAndDistinctFromAIStudio(t *testing.T) {
	models := vertexCuratedModels()
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	// The same underlying model carries different ids on each surface; using
	// AI Studio's id against Vertex 404s.
	// veo-3.1-lite-generate-001 is the id that actually resolves on Vertex;
	// veo-3.0-generate-001 (Model Garden's card) 404s on this surface.
	if !ids["veo-3.1-lite-generate-001"] {
		t.Fatal("vertex catalogue is missing the Veo id that resolves")
	}
	if ids["veo-3.1-fast-generate-preview"] {
		t.Fatal("vertex catalogue must not carry AI Studio ids")
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
	_, err := NewVertexAI(server.URL, "k", "p", "global", 5).Complete(
		"gemini-3.5-flash", []Message{{"role": "user", "content": "hi"}}, Kwargs{"max_tokens": 32})
	if err == nil {
		t.Fatal("an empty reply must not be reported as success")
	}

	for _, want := range []string{"reasoning", "29 thinking tokens", "raise max_tokens"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

func TestGoogleUsageIncludesThinkingTokens(t *testing.T) {
	input, output, total := googleUsage(map[string]any{
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(7),
			"candidatesTokenCount": float64(3),
			"thoughtsTokenCount":   float64(29),
			"totalTokenCount":      float64(39),
		},
	})
	if input != 7 || output != 32 || total != 39 {
		t.Fatalf("usage=(%d,%d,%d)", input, output, total)
	}

}

func TestGoogleMapsDeveloperMessageToSystemInstruction(t *testing.T) {
	payload := googleContentRequest([]Message{
		{"role": "developer", "content": "developer policy"},
		{"role": "user", "content": "hello"},
	}, Kwargs{}, nil)
	system, _ := payload["systemInstruction"].(map[string]any)
	parts, _ := system["parts"].([]map[string]any)
	if len(parts) != 1 || parts[0]["text"] != "developer policy" {
		t.Fatalf("systemInstruction=%+v", system)
	}
	contents, _ := payload["contents"].([]map[string]any)
	if len(contents) != 1 || contents[0]["role"] != "user" {
		t.Fatalf("contents=%+v", contents)
	}
}

func TestEmptyReplyWithoutThinkingReportsFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model"},"finishReason":"SAFETY"}]}`))
	}))
	defer server.Close()
	_, err := NewAIStudio(server.URL, "k", 5).Complete("m", []Message{{"role": "user", "content": "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "SAFETY") {
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
