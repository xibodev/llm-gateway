package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// ollamaProvider is the gateway's Ollama facade. Chat and its stream go
// through the core Runtime, whose core Ollama converts each request to the
// daemon's native /api/chat and each answer back, as the gateway's
// transport did, roles and tool calls included. The catalog stays on the
// gateway's path, read by a core Ollama of the facade's own, because the
// Runtime keeps no catalog of these instances.
type ollamaProvider struct {
	runtime  *Runtime
	instance string
	caller   core.Caller
	catalog  *coreproviders.Ollama
}

var _ Provider = (*ollamaProvider)(nil)

// newOllamaProvider returns the facade of the Ollama instance at base for
// caller. A base that is not a daemon root builds none.
func (rt *Runtime) newOllamaProvider(instance string, caller core.Caller, base string, timeout float64) (*ollamaProvider, error) {
	catalog, err := newCoreOllama(instance, base, timeout)
	if err != nil {
		return nil, err
	}
	return &ollamaProvider{runtime: rt, instance: instance, caller: caller, catalog: catalog}, nil
}

func ollamaProcessBoundaryGuidance(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "127.0.0.1" || host == "localhost" || host == "::1" {
		return "Loopback reaches Ollama only from the gateway's own network boundary; a container must use a host-reachable address such as host.docker.internal."
	}
	return ""
}

func (p *ollamaProvider) IsStub() bool { return false }

func (p *ollamaProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *ollamaProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	request, err := ollamaRequest(model, messages, kw)
	if err != nil {
		return nil, err
	}
	ctx = withCoreOperation(ctx, ollamaCoreType, p.caller)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, ollamaFailure(ctx, err)
	}
	var completion map[string]any
	if err := json.Unmarshal(response.Body, &completion); err != nil {
		return nil, circuitFailureInvocation("ollama: invalid response payload")
	}
	return completion, nil
}

func (p *ollamaProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

// StreamContext relays Ollama's stream as Chat chunks. It still finishes
// with stop after tool calls, as the transport's did; the API layer's chat
// stream rewrites that finish to tool_calls.
func (p *ollamaProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	request, err := ollamaRequest(model, messages, kw)
	if err != nil {
		return nil, err
	}
	ctx = withCoreOperation(ctx, ollamaCoreType, p.caller)
	stream, err := p.runtime.core.Stream(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, ollamaFailure(ctx, err)
	}
	return &ollamaCoreStream{ctx: ctx, inner: stream}, nil
}

// ollamaRequest is the Chat body core's Ollama reads: the model, the
// messages and the options. Keys that begin with an underscore are the
// gateway's own hints, not Chat fields, and stay out, except the output
// limit of a Responses request, which the transport sent as num_predict
// when max_tokens was unset, and so is sent as max_tokens.
func ollamaRequest(model string, messages []Message, kw Kwargs) (core.Request, error) {
	payload := make(map[string]any, len(kw)+2)
	for key, value := range kw {
		if !strings.HasPrefix(key, "_") {
			payload[key] = value
		}
	}
	if payload["max_tokens"] == nil && kw["_max_output_tokens"] != nil {
		payload["max_tokens"] = kw["_max_output_tokens"]
	}
	payload["model"], payload["messages"] = model, messages
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, invocation("ollama: request encoding failed")
	}
	return core.Request{Surface: core.ModelSurfaceChatCompletions, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
}

// ollamaFailure returns the gateway error for what the core Runtime returned
// for an Ollama operation, so the router and the resilience wrapper decide
// on it as they did on the transport's errors. A refusal keeps the daemon's
// status and, in the transport's message, its words, which a client reads
// when the refusal ends the chain; the transport read no Retry-After, so
// none is kept. A request that got no complete answer may repeat, and an
// answer that cannot be used counts against the circuit.
func ollamaFailure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	var refusal *coreproviders.InvocationError
	if errors.As(err, &refusal) && refusal.Status != 0 {
		// Core quotes the daemon as "Ollama: upstream returned <status>:
		// <words>", already redacted and bounded.
		words := strings.TrimPrefix(refusal.Msg, fmt.Sprintf("Ollama: upstream returned %d: ", refusal.Status))
		return invocationStatus(fmt.Sprintf("ollama: request failed (%d): %s", refusal.Status, words), refusal.Status)
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		return invocation("ollama: shared transport failed")
	}
	message := "ollama: " + failure.Message
	switch {
	case failure.Class != core.ProviderErrorTransport && failure.Class != core.ProviderErrorUpstream:
		// A request core would not send, or a provider it could not build.
		return &ConfigError{Msg: message}
	case failure.Classification.Retryable:
		return retryableInvocation(message)
	case failure.Classification.CircuitFailure:
		return circuitFailureInvocation(message)
	}
	return invocation(message)
}

// ollamaCoreStream relays core's Ollama stream as the chunks the API layer
// reads, as the transport's stream returned them. Each frame is one data
// record of one Chat chunk, and [DONE] ends the stream.
type ollamaCoreStream struct {
	ctx   context.Context
	inner core.StreamIter
	err   error
}

func (s *ollamaCoreStream) Next() (string, bool) {
	for {
		frame, err := s.inner.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.err = s.failure(err)
			}
			return "", false
		}
		data := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(frame)), "data:"))
		if data != "" && data != "[DONE]" {
			return data, true
		}
	}
}

func (s *ollamaCoreStream) Err() error   { return s.err }
func (s *ollamaCoreStream) Close() error { return s.inner.Close() }

// failure reports what ended the stream. Of the failures core's stream
// reports once open, only a line over the wire limit is an upstream one
// with a cause, and the transport reported that as the record it would not
// read.
func (s *ollamaCoreStream) failure(err error) error {
	var failure *core.ProviderError
	if s.ctx.Err() == nil && errors.As(err, &failure) && failure.Class == core.ProviderErrorUpstream && failure.Cause != nil {
		return &StreamRecordTooLargeError{Format: "NDJSON", Limit: maxStreamRecordWireSize}
	}
	return ollamaFailure(s.ctx, err)
}

func (p *ollamaProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the models the daemon holds, as core's Ollama
// reads /api/tags: each named by its name, or else its model, and made by
// its family, or else ollama.
func (p *ollamaProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	models, err := p.catalog.ListModels(context.Background(), nil)
	if err != nil {
		return nil, nil, ollamaCatalogError(err)
	}
	rows := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		rows = append(rows, ModelInfo{ID: model.ID, Vendor: model.Vendor})
	}
	return rows, nil, nil
}

// ollamaCatalogError is the gateway's catalog failure for core's. Core's
// codes are the gateway's without their catalog_ prefix, and its details
// are the gateway's words, so the failure reads as the transport's did.
func ollamaCatalogError(err error) error {
	var failure *coreproviders.CatalogError
	if !errors.As(err, &failure) {
		return catalogError("catalog_failed", "Provider catalog failed.", 0)
	}
	return catalogError("catalog_"+failure.Code, failure.Detail, failure.Status)
}

// intOf reads an integer option. Request handlers decode bodies with
// UseNumber, so json.Number must count; ignoring it silently replaced a
// caller's max_tokens with the provider default (llm-gateway#77).
func intOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if value, err := n.Int64(); err == nil {
			return int(value)
		}
		if value, err := n.Float64(); err == nil {
			return int(value)
		}
	}
	return 0
}
