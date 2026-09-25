package providers

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnonymousProviderProfilesMatchReviewedRegistry(t *testing.T) {
	profiles := AnonymousProviderProfiles()
	if len(profiles) != 5 {
		t.Fatalf("profiles=%+v", profiles)
	}
	want := []string{"kilo_code", "llm7", "opencode_zen", "ovh_ai_endpoints", "pollinations"}
	for index, profile := range profiles {
		if profile.RegistryID != want[index] || profile.ProviderID == "" || profile.BaseURL == "" || len(profile.VerificationModels) == 0 {
			t.Fatalf("profile[%d]=%+v", index, profile)
		}
	}
}

func TestAnonymousCatalogProfilesFilterClaimedModels(t *testing.T) {
	rows := []ModelInfo{{ID: "free"}, {ID: "paid"}, {ID: "media"}}
	for _, testCase := range []struct {
		registry string
		items    []any
		want     string
	}{
		{"kilo_code", []any{
			map[string]any{"id": "free", "isFree": true, "pricing": map[string]any{"prompt": "0", "completion": "0"}, "architecture": map[string]any{"output_modalities": []any{"text"}}},
			map[string]any{"id": "paid", "isFree": false, "pricing": map[string]any{"prompt": "1", "completion": "1"}, "architecture": map[string]any{"output_modalities": []any{"text"}}},
			map[string]any{"id": "media", "isFree": true, "pricing": map[string]any{"prompt": "0", "completion": "0"}, "architecture": map[string]any{"output_modalities": []any{"image"}}},
		}, "free"},
		{"llm7", []any{
			map[string]any{"id": "free", "tier": "turbo", "usage_based_only": false, "model_type": "chat", "schema_endpoints": []any{"openai"}},
			map[string]any{"id": "paid", "tier": "pro", "usage_based_only": true, "model_type": "chat", "schema_endpoints": []any{"openai"}},
			map[string]any{"id": "media", "tier": "turbo", "usage_based_only": false, "model_type": "image", "schema_endpoints": []any{"openai"}},
		}, "free"},
		{"ovh_ai_endpoints", []any{
			map[string]any{"id": "free", "context_length": float64(1024), "max_completion_tokens": float64(128), "pricing": map[string]any{"prompt": "0", "completion": "0"}},
			map[string]any{"id": "paid", "context_length": float64(1024), "max_completion_tokens": float64(128), "pricing": map[string]any{"prompt": "1", "completion": "1"}},
			map[string]any{"id": "media", "context_length": float64(0), "max_completion_tokens": float64(0), "pricing": map[string]any{"prompt": "0", "completion": "0"}},
		}, "free"},
	} {
		t.Run(testCase.registry, func(t *testing.T) {
			catalog := openAICatalog{registryID: testCase.registry, anonymous: true}
			got, err := catalog.normalizeAnonymousCatalog(rows, testCase.items)
			if err != nil || len(got) != 1 || got[0].ID != testCase.want || !got[0].Free {
				t.Fatalf("rows=%+v err=%v", got, err)
			}
		})
	}
}

func TestPollinationsProfileUsesDistinctPathsAndArrayCatalog(t *testing.T) {
	provider := OpenAIProvider{registryID: "pollinations", anonymous: true}
	catalog := openAICatalog{registryID: "pollinations", anonymous: true}
	if catalog.modelsURL("https://text.pollinations.ai") != "https://text.pollinations.ai/models" ||
		provider.chatURL("https://text.pollinations.ai") != "https://text.pollinations.ai/v1/chat/completions" {
		t.Fatalf("models=%q chat=%q", catalog.modelsURL("https://text.pollinations.ai"), provider.chatURL("https://text.pollinations.ai"))
	}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Body: io.NopCloser(strings.NewReader(`[
			{"name":"openai-fast","tier":"anonymous","output_modalities":["text"],"tools":true},
			{"name":"paid","tier":"paid","output_modalities":["text"]},
			{"name":"image","tier":"anonymous","output_modalities":["image"]}
		]`)),
	}
	rows, err := decodePollinationsCatalog(response, true)
	if err != nil || len(rows) != 1 || rows[0].ID != "openai-fast" || !rows[0].Free {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestPollinationsCatalogRejectsNull(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`null`))}
	rows, err := decodePollinationsCatalog(response, true)
	if rows != nil || err == nil {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}

func TestAnonymousVerificationModelNeverFallsBackToUnmarkedRows(t *testing.T) {
	rows := []ModelInfo{{ID: "paid"}, {ID: "mistral-Nemo-Instruct-2407", Free: true}, {ID: "codestral-latest", Free: true}}
	if got := AnonymousVerificationModel("llm7", rows); got != "codestral-latest" {
		t.Fatalf("model=%q", got)
	}
	if got := AnonymousVerificationModel("llm7", []ModelInfo{{ID: "paid"}}); got != "" {
		t.Fatalf("unmarked fallback=%q", got)
	}
}
