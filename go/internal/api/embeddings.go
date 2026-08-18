package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// POST /v1/embeddings — text -> vector. Reverse-proxied to the resolved
// OpenAI-compatible provider (e.g. LocalAI's llama-cpp embedding backend).
//
// WHY THIS IS A PROXY AND NOT A Provider METHOD
// ----------------------------------------------
// Embeddings are a transport-shaped capability, not a conversation: there are no
// messages, no streaming, no tools, and no failover semantics worth having —
// falling back to a *different* embedding model would silently return vectors
// from a different vector space, which is worse than an error because the caller
// cannot detect it. A vector index built with model A and queried with model B
// returns plausible, ranked, wrong results and raises nothing.
//
// So this deliberately follows the audio precedent in `audio.go` (single resolved
// target, reverse proxy through ProviderHTTPTarget) rather than the chat
// precedent in `openai.go` (failover chain through router.ExecuteComplete).
// Category-style failover across providers is NOT offered here on purpose.
//
// It still goes through the full governance path — `authed`, then
// `ResolveForPrincipal`, then `enforceKeyPolicy` (project/key allowlists,
// provider authorisation, and the durable RPM/day/month/budget consumption) —
// so an embeddings call is metered and refused exactly like any other.

var embeddingsClient = &http.Client{Timeout: 120 * time.Second}

type embeddingsRequest struct {
	Model          string `json:"model"`
	Input          any    `json:"input"`
	EncodingFormat string `json:"encoding_format,omitempty"`
	Dimensions     any    `json:"dimensions,omitempty"`
	User           string `json:"user,omitempty"`
}

// embeddingsCreditsMilli is what one embeddings request costs against a key's
// credit budget.
//
// The default in `router.RecordUsage` is 1000 milli-credits — "one neutral
// model-credit" — applied to any successful non-stub request whose record does
// not set the field. That default is calibrated for a chat completion. An
// embedding of a single search query is three orders of magnitude cheaper in
// every real dimension (one forward pass over ~12 tokens, no generation, no KV
// cache growth), and query-time embedding means ONE CALL PER USER SEARCH.
// Charging a full model-credit for it would drain a project's budget at chat
// rates for near-zero real cost, which is a metering bug that would only surface
// as unexplained 429s.
//
// 10 is deliberately non-zero: `RecordUsage` treats 0 as "unset" and substitutes
// 1000, so zero is not expressible, and a visible small charge is better than a
// hidden default anyway.
const embeddingsCreditsMilli = 10

func handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var req embeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		recordFailureUsage("openai.embeddings", "", principal, 422, "invalid_body", started)
		writeError(w, 422, "invalid request body")
		return
	}
	if inputEmpty(req.Input) {
		recordFailureUsage("openai.embeddings", req.Model, principal, 400, "missing_input", started)
		writeError(w, 400, "'input' is required (a string or an array of strings)")
		return
	}

	// resolveAudioTarget is misnamed for this use but is exactly the right
	// function: it resolves provider/model or a category to ONE target and runs
	// the key policy over it. Duplicating it here would mean two copies of the
	// policy call that could drift apart.
	provider, upstreamModel, status, msg := resolveAudioTarget(principal, req.Model)
	if status != 0 {
		if status == 400 && strings.TrimSpace(req.Model) == "" {
			msg = "'model' is required (e.g. llama-embed/qwen3-embedding-0.6b)"
		}
		recordFailureUsage("openai.embeddings", req.Model, principal, status, "policy_or_route", started)
		writeError(w, status, msg)
		return
	}

	base, headers, okp := providers.ProviderHTTPTarget(provider, principal)
	if !okp {
		recordFailureUsage("openai.embeddings", req.Model, principal, 400, "embeddings_unsupported", started)
		writeError(w, 400, "provider '"+provider+"' does not support embeddings "+
			"(use an openai_compatible provider such as llama-embed)")
		return
	}

	// Re-marshal from the typed struct rather than forwarding the raw body: it
	// drops unknown fields, and it is what rewrites `model` from the gateway's
	// namespaced `provider/model` to the bare id the upstream knows.
	req.Model = upstreamModel
	payload, _ := json.Marshal(req)
	upstream, _ := http.NewRequest("POST", base+"/embeddings", bytes.NewReader(payload))
	copyAuthHeaders(upstream, headers, false)
	upstream.Header.Set("Content-Type", "application/json")

	resp, err := embeddingsClient.Do(upstream)
	if err != nil {
		recordFailureUsage("openai.embeddings", req.Model, principal, 502, "upstream", started)
		writeError(w, 502, "embeddings upstream error")
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// An embeddings response reports `usage.prompt_tokens` and `usage.total_tokens`
	// and has NO `completion_tokens`. `usage_events.output_tokens` is NOT NULL
	// DEFAULT 0 and the Go field is a plain int, so leaving it zero is correct and
	// needs no schema change — an embedding produces no output tokens because it
	// generates nothing.
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.embeddings", RequestedModel: provider + "/" + upstreamModel,
		RoutedModel: upstreamModel, Provider: provider,
		Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID,
		KeyID: principal.KeyID, InputTokens: embeddingsPromptTokens(body),
		StatusCode: resp.StatusCode, LatencyMS: time.Since(started).Milliseconds(),
		ErrorCode: audioErrorCode(resp.StatusCode), IsStub: isStub(provider),
		CreditsMilli: embeddingsCreditsMilli,
	})

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// inputEmpty rejects the three shapes that mean "nothing to embed" before a
// round trip: absent, an empty string, and an empty array. A silently-empty
// input is otherwise answered by a 200 carrying zero vectors, which a caller
// reads as success.
func inputEmpty(input any) bool {
	switch v := input.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	}
	return false
}

// embeddingsPromptTokens reads the token count the upstream reported. Absent
// (LocalAI does not always send `usage`) it returns 0, which records the request
// without inventing a number — the alternative, estimating from character count,
// would put a fabricated figure into the billing ledger.
func embeddingsPromptTokens(body []byte) int {
	var parsed struct {
		Usage map[string]any `json:"usage"`
	}
	if json.Unmarshal(body, &parsed) != nil || parsed.Usage == nil {
		return 0
	}
	return firstInt(parsed.Usage, "prompt_tokens", "input_tokens", "total_tokens")
}

// embeddingModelHints mark a catalogue entry as an embedding model when the
// provider reports no capabilities of its own. LocalAI's `/v1/models` returns
// bare `{"id": ...}` — no capabilities, no supported_endpoints — so without this
// its embedding model appears in `GET /v1/models` advertising nothing, which is
// how `localai/granite-embedding-107m-multilingual` came to be listed by the
// gateway and reachable through no endpoint at all.
var embeddingModelHints = []string{
	"embedding", "embed-", "-embed", "embeddinggemma", "bge-", "gte-",
	"e5-", "nomic-embed", "text-embedding",
}

func looksLikeEmbeddingModel(haystack string) bool {
	for _, hint := range embeddingModelHints {
		if strings.Contains(haystack, hint) {
			return true
		}
	}
	return false
}

var _ = config.Principal{}
