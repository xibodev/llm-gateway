package providers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
)

func TestOllamaStreamNormalAndOversizedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantText string
		wantErr  bool
	}{
		{name: "normal", response: `{"message":{"content":"hello"},"done":false}` + "\n" + `{"done":true}` + "\n", wantText: "hello"},
		{name: "oversized", response: strings.Repeat("x", maxStreamRecordWireSize+1) + "\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			iter, err := (OllamaProvider{BaseURL: server.URL, Timeout: 2}).Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer iter.Close()
			var chunks strings.Builder
			for chunk, ok := iter.Next(); ok; chunk, ok = iter.Next() {
				chunks.WriteString(chunk)
			}
			if tc.wantText != "" && !strings.Contains(chunks.String(), tc.wantText) {
				t.Fatalf("chunks = %s", chunks.String())
			}
			var sizeErr *StreamRecordTooLargeError
			if errors.As(iter.Err(), &sizeErr) != tc.wantErr {
				t.Fatalf("error = %#v, want oversized %v", iter.Err(), tc.wantErr)
			}
			if sizeErr != nil && (sizeErr.Format != "NDJSON" || strings.Contains(sizeErr.Error(), strings.Repeat("x", 32))) {
				t.Fatalf("unsafe error = %#v", sizeErr)
			}
		})
	}
}

func TestOllamaNativeRootDiagnostics(t *testing.T) {
	for _, scenario := range []struct {
		base, wantIssue string
	}{
		{base: "http://127.0.0.1:11434"},
		{base: "http://host.docker.internal:11434"},
		{base: "http://127.0.0.1:11434/v1", wantIssue: "not the OpenAI-compatible /v1 URL"},
		{base: "http://127.0.0.1:11434/api", wantIssue: "with no path"},
		{base: "127.0.0.1:11434", wantIssue: "must be an http(s)"},
	} {
		t.Run(scenario.base, func(t *testing.T) {
			issue := ollamaBaseURLIssue(scenario.base)
			if scenario.wantIssue == "" && issue != "" || scenario.wantIssue != "" && !strings.Contains(issue, scenario.wantIssue) {
				t.Fatalf("issue=%q", issue)
			}
		})
	}
	if guidance := ollamaProcessBoundaryGuidance("http://localhost:11434"); !strings.Contains(guidance, "container") || !strings.Contains(guidance, "host.docker.internal") {
		t.Fatalf("loopback guidance=%q", guidance)
	}
	if guidance := ollamaProcessBoundaryGuidance("http://ollama:11434"); guidance != "" {
		t.Fatalf("non-loopback guidance=%q", guidance)
	}
}

func TestOllamaConfigurationIssueRejectsOpenAICompatiblePath(t *testing.T) {
	oldProviders := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"ollama-fixture": {Type: "ollama", BaseURL: "http://127.0.0.1:11434/v1"},
		}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	if issue := ProviderConfigurationIssue("ollama-fixture"); !strings.Contains(issue, "remove /v1") {
		t.Fatalf("configuration issue=%q", issue)
	}
}
