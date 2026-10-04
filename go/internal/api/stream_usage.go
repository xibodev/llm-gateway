package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/router"
)

// streamUsage is what a stream consumed from the moment its first byte went
// out. Every way the stream ends records it with the provider and model that
// served it: a stream that completed, one its upstream failed and one its
// client left all consumed a model, so a budget never sees one as free.
type streamUsage struct {
	endpoint, requested string
	principal           *config.Principal
	served              *router.Target
	started             time.Time
	// promptBytes is the size of what was sent, the prompt estimate's base.
	promptBytes int
	// input and output are the last counts the upstream reported, zero
	// until it reports one.
	input, output int
	// streamed estimates the output tokens of the text streamed so far.
	streamed int
}

func newStreamUsage(endpoint, requested string, principal *config.Principal, served *router.Target, started time.Time, promptBytes int) *streamUsage {
	return &streamUsage{
		endpoint: endpoint, requested: requested, principal: principal,
		served: served, started: started, promptBytes: promptBytes,
	}
}

// estimatedTokens is the token estimate for n bytes of JSON or text: one
// token per four bytes, rounded up. Token counting estimates a prompt the
// same way.
func estimatedTokens(n int) int {
	return (n + 3) / 4
}

// streamedText adds the tokens of one streamed piece of text, estimated as
// llm-translate estimates a stream's output: a quarter of its bytes, and at
// least one, since a streamed piece is seldom less than a token.
func (u *streamUsage) streamedText(piece any) {
	if text, ok := piece.(string); ok && text != "" {
		u.streamed += max(1, len(text)/4)
	}
}

// reported keeps the counts an upstream reported in usage. A zero is not a
// report: some upstreams send zeros until their last chunk.
func (u *streamUsage) reported(usage map[string]any) {
	if value := firstInt(usage, "prompt_tokens", "input_tokens"); value > 0 {
		u.input = value
	}
	if value := firstInt(usage, "completion_tokens", "output_tokens"); value > 0 {
		u.output = value
	}
}

// chatChunk reads one Chat chunk: its usage, and the text its choices
// stream, which is what the client was sent.
func (u *streamUsage) chatChunk(chunk string) {
	var parsed map[string]any
	if json.Unmarshal([]byte(chunk), &parsed) == nil {
		u.chat(parsed)
	}
}

// chat reads a Chat chunk already decoded.
func (u *streamUsage) chat(chunk map[string]any) {
	if usage, ok := chunk["usage"].(map[string]any); ok {
		u.reported(usage)
	}
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		u.streamedText(delta["content"])
		u.streamedText(delta["refusal"])
		// Upstreams name streamed reasoning either way, and some send both
		// with the same text.
		if reasoning, _ := delta["reasoning_content"].(string); reasoning != "" {
			u.streamedText(reasoning)
		} else {
			u.streamedText(delta["reasoning"])
		}
		calls, _ := delta["tool_calls"].([]any)
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			function, _ := call["function"].(map[string]any)
			u.streamedText(function["arguments"])
		}
	}
}

// responsesEvent reads one Responses event: the usage of the response it
// carries, and the text of a delta. An audio delta is encoded audio, not
// text, and is left out.
func (u *streamUsage) responsesEvent(event map[string]any) {
	if response, ok := event["response"].(map[string]any); ok {
		if usage, ok := response["usage"].(map[string]any); ok {
			u.reported(usage)
		}
	}
	if eventType, _ := event["type"].(string); strings.HasSuffix(eventType, ".delta") && eventType != "response.audio.delta" {
		u.streamedText(event["delta"])
	}
}

// tokens are the counts the upstream reported, each estimated where it
// reported none: the prompt from the size of what was sent, the output
// from the text streamed.
func (u *streamUsage) tokens() (int, int) {
	input, output := u.input, u.output
	if input == 0 {
		input = estimatedTokens(u.promptBytes)
	}
	if output == 0 {
		output = u.streamed
	}
	return input, output
}

func (u *streamUsage) record(status int, errorCode string) {
	input, output := u.tokens()
	router.RecordUsage(router.UsageRecord{
		Endpoint: u.endpoint, RequestedModel: u.requested, RoutedModel: u.served.Model,
		Provider: u.served.Provider, Project: u.principal.Project, Key: u.principal.Key,
		ProjectID: u.principal.ProjectID, PrincipalID: u.principal.PrincipalID, KeyID: u.principal.KeyID,
		InputTokens: input, OutputTokens: output, StatusCode: status, ErrorCode: errorCode,
		LatencyMS: time.Since(u.started).Milliseconds(), IsStub: isStub(u.served.Provider),
	})
}

// completed records a stream that ended with the surface's terminal event.
func (u *streamUsage) completed() { u.record(http.StatusOK, "") }

// failed records a stream its upstream failed or ended early.
func (u *streamUsage) failed() { u.record(http.StatusBadGateway, "upstream_stream") }

// cancelled records a stream its client left, by disconnecting or by a
// write that failed.
func (u *streamUsage) cancelled() { u.record(499, "client_cancelled") }
