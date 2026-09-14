package providers

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

type retryResponsesProvider struct {
	completeCalls int
	streamCalls   int
	status        int
	malformed     bool
}

type resilientModalityProvider struct {
	speechCalls     int
	imageCalls      int
	videoStartCalls int
	videoPollCalls  int
}

func (provider *resilientModalityProvider) IsStub() bool            { return false }
func (provider *resilientModalityProvider) ListModels() []ModelInfo { return nil }
func (provider *resilientModalityProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, errors.New("unused")
}
func (provider *resilientModalityProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, errors.New("unused")
}
func (provider *resilientModalityProvider) DefaultVoice() string { return "voice" }
func (provider *resilientModalityProvider) Synthesize(string, string, string) ([]byte, string, error) {
	provider.speechCalls++
	if provider.speechCalls == 1 {
		return nil, "", retryableInvocation("speech transport")
	}
	return []byte("audio"), "mp3", nil
}
func (provider *resilientModalityProvider) GenerateImages(string, string, int) ([]GeneratedImage, map[string]any, error) {
	provider.imageCalls++
	if provider.imageCalls == 1 {
		return nil, nil, retryableInvocation("image transport")
	}
	return []GeneratedImage{{Data: []byte("image"), MimeType: "image/png"}}, nil, nil
}
func (provider *resilientModalityProvider) StartVideo(string, string, map[string]any) (VideoJob, error) {
	provider.videoStartCalls++
	return VideoJob{}, retryableInvocation("video start transport")
}
func (provider *resilientModalityProvider) PollVideo(string) (VideoJob, error) {
	provider.videoPollCalls++
	if provider.videoPollCalls == 1 {
		return VideoJob{}, retryableInvocation("video poll transport")
	}
	return VideoJob{Operation: "operation", Done: true}, nil
}

func (provider *retryResponsesProvider) IsStub() bool { return false }
func (provider *retryResponsesProvider) ListModels() []ModelInfo {
	return []ModelInfo{}
}
func (provider *retryResponsesProvider) Complete(
	string, []Message, Kwargs,
) (map[string]any, error) {
	return nil, errors.New("unused")
}
func (provider *retryResponsesProvider) Stream(
	string, []Message, Kwargs,
) (StreamIter, error) {
	return nil, errors.New("unused")
}
func (provider *retryResponsesProvider) CompleteResponses(
	string, map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	provider.completeCalls++
	if provider.malformed {
		return nil, nil, circuitFailureInvocation("invalid upstream response")
	}
	if provider.status == 0 {
		return nil, nil, retryableInvocation("upstream transport failure")
	}
	return nil, nil, invocationStatus("upstream failure", provider.status)
}
func (provider *retryResponsesProvider) StreamResponses(
	string, map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	provider.streamCalls++
	if provider.malformed {
		return nil, nil, circuitFailureInvocation("invalid upstream response")
	}
	if provider.status == 0 {
		return nil, nil, retryableInvocation("upstream transport failure")
	}
	return nil, nil, invocationStatus("upstream failure", provider.status)
}

func (provider *retryResponsesProvider) CompleteContext(
	context.Context, string, []Message, Kwargs,
) (map[string]any, error) {
	provider.completeCalls++
	if provider.malformed {
		return nil, circuitFailureInvocation("invalid upstream response")
	}
	if provider.status == 0 {
		return nil, retryableInvocation("upstream transport failure")
	}
	return nil, invocationStatus("upstream failure", provider.status)
}

func (provider *retryResponsesProvider) StreamContext(
	context.Context, string, []Message, Kwargs,
) (StreamIter, error) {
	provider.streamCalls++
	if provider.malformed {
		return nil, circuitFailureInvocation("invalid upstream response")
	}
	if provider.status == 0 {
		return nil, retryableInvocation("upstream transport failure")
	}
	return nil, invocationStatus("upstream failure", provider.status)
}

func TestV043StatefulResponsesDisableRetries(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"previous response": {
			"input": "hello", "previous_response_id": "resp_private",
		},
		"conversation": {
			"input": "hello", "conversation": "conv_private",
		},
		"vector store": {
			"input": "hello",
			"tools": []any{map[string]any{
				"type":             "file_search",
				"vector_store_ids": []any{"vs_private"},
			}},
		},
		"stored response": {
			"input": "hello", "store": true,
		},
		"hosted tool": {
			"input": "hello",
			"tools": []any{map[string]any{
				"type": "mcp", "server_label": "write-server",
			}},
		},
		"mcp approval": {
			"input": []any{map[string]any{
				"type": "mcp_approval_response", "approve": true,
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &retryResponsesProvider{}
			provider := &ResilientProvider{
				inner: inner,
				policy: config.ProviderPolicy{
					RetryMaxAttempts: 3,
				},
			}
			_, _, _ = provider.CompleteResponses("model", payload)
			_, _, _ = provider.StreamResponses("model", payload)
			if inner.completeCalls != 1 || inner.streamCalls != 1 {
				t.Fatalf(
					"calls complete=%d stream=%d",
					inner.completeCalls, inner.streamCalls,
				)
			}
		})
	}
}

func TestV043ResilientProviderRetriesOnlyTransientInvocationFailures(t *testing.T) {
	for _, status := range []int{0, 408, 429, 500, 502, 503, 504, 400, 401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			want := 1
			retryable := invocationStatus("failure", status)
			if status == 0 {
				retryable = retryableInvocation("transport failure")
			}
			if InvocationRetryable(retryable) {
				want = 3
			}
			for _, surface := range []string{"chat", "chat stream", "responses", "responses stream"} {
				t.Run(surface, func(t *testing.T) {
					inner := &retryResponsesProvider{status: status}
					provider := &ResilientProvider{
						inner: inner,
						name:  t.Name(),
						policy: config.ProviderPolicy{
							RetryMaxAttempts: 3,
						},
					}
					switch surface {
					case "chat":
						_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
					case "chat stream":
						_, _ = provider.StreamContext(context.Background(), "model", nil, nil)
					case "responses":
						_, _, _ = provider.CompleteResponsesContext(context.Background(), "model", map[string]any{})
					case "responses stream":
						_, _, _ = provider.StreamResponsesContext(context.Background(), "model", map[string]any{})
					}
					calls := inner.completeCalls
					if surface == "chat stream" || surface == "responses stream" {
						calls = inner.streamCalls
					}
					if calls != want {
						t.Fatalf("calls=%d want=%d", calls, want)
					}
				})
			}
		})
	}
}

func TestV043DefinitiveUpstreamFailureDoesNotOpenCircuit(t *testing.T) {
	inner := &retryResponsesProvider{status: 400}
	provider := &ResilientProvider{
		inner: inner,
		name:  t.Name(),
		policy: config.ProviderPolicy{
			RetryMaxAttempts:        3,
			CircuitFailureThreshold: 1,
			CircuitCooldownSeconds:  60,
		},
	}
	for range 2 {
		_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	}
	if inner.completeCalls != 2 {
		t.Fatalf("complete calls=%d want=2", inner.completeCalls)
	}
}

func TestV043DefinitiveUpstreamFailureBreaksTransientCircuitStreak(t *testing.T) {
	ResetCircuit(t.Name())
	t.Cleanup(func() { ResetCircuit(t.Name()) })
	inner := &retryResponsesProvider{status: 503}
	provider := &ResilientProvider{
		inner: inner,
		name:  t.Name(),
		policy: config.ProviderPolicy{
			RetryMaxAttempts:        1,
			CircuitFailureThreshold: 2,
			CircuitCooldownSeconds:  60,
		},
	}
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	inner.status = 400
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	inner.status = 503
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	if inner.completeCalls != 4 {
		t.Fatalf("complete calls=%d want=4; non-retryable 400 did not reset the transient streak", inner.completeCalls)
	}
}

func TestV043StatuslessInvocationRequiresExplicitRetryability(t *testing.T) {
	malformed := invocation("invalid upstream response")
	if InvocationRetryable(malformed) {
		t.Fatal("ordinary statusless invocation must not retry")
	}
	if !InvocationFailoverEligible(malformed) {
		t.Fatal("ordinary statusless invocation must allow endpoint failover")
	}
	if !InvocationRetryable(retryableInvocation("transport failure")) {
		t.Fatal("explicit transport invocation must retry")
	}
	if InvocationFailoverEligible(invocationStatus("bad request", 400)) {
		t.Fatal("definitive upstream 400 must not allow native Messages failover")
	}
}

func TestV043MalformedResponseOpensCircuitWithoutRetry(t *testing.T) {
	ResetCircuit(t.Name())
	t.Cleanup(func() { ResetCircuit(t.Name()) })
	inner := &retryResponsesProvider{malformed: true}
	provider := &ResilientProvider{inner: inner, name: t.Name(), policy: config.ProviderPolicy{
		RetryMaxAttempts: 3, CircuitFailureThreshold: 1, CircuitCooldownSeconds: 60,
	}}
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	_, _ = provider.CompleteContext(context.Background(), "model", nil, nil)
	if inner.completeCalls != 1 {
		t.Fatalf("complete calls=%d want=1", inner.completeCalls)
	}
}

func TestV043NativeModalitiesPreserveResiliencePolicy(t *testing.T) {
	inner := &resilientModalityProvider{}
	provider := &ResilientProvider{inner: inner, name: t.Name(), policy: config.ProviderPolicy{
		RetryMaxAttempts: 2, RetryInitialBackoffSeconds: 0, RetryMaxBackoffSeconds: 0,
	}}
	speech, ok := AsSpeechSynthesizer(provider)
	if !ok {
		t.Fatal("speech capability was unwrapped past resilience")
	}
	if _, _, err := speech.Synthesize("", "hello", ""); err != nil || inner.speechCalls != 2 {
		t.Fatalf("speech error=%v calls=%d", err, inner.speechCalls)
	}
	image, ok := AsImageGenerator(provider)
	if !ok {
		t.Fatal("image capability was unwrapped past resilience")
	}
	if _, _, err := image.GenerateImages("model", "prompt", 1); err == nil || inner.imageCalls != 1 {
		t.Fatalf("image error=%v calls=%d", err, inner.imageCalls)
	}
	ResetCircuit(t.Name())
	video, ok := AsVideoGenerator(provider)
	if !ok {
		t.Fatal("video capability was unwrapped past resilience")
	}
	if _, err := video.StartVideo("model", "prompt", nil); err == nil || inner.videoStartCalls != 1 {
		t.Fatalf("video start error=%v calls=%d", err, inner.videoStartCalls)
	}
	ResetCircuit(t.Name())
	if _, err := video.PollVideo("operation"); err != nil || inner.videoPollCalls != 2 {
		t.Fatalf("video poll error=%v calls=%d", err, inner.videoPollCalls)
	}
	unsupported := &ResilientProvider{inner: EchoProvider{}, name: "unsupported", policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
	if _, ok := AsSpeechSynthesizer(unsupported); ok {
		t.Fatal("resilience wrapper invented speech capability")
	}
	if _, ok := AsImageGenerator(unsupported); ok {
		t.Fatal("resilience wrapper invented image capability")
	}
	if _, ok := AsVideoGenerator(unsupported); ok {
		t.Fatal("resilience wrapper invented video capability")
	}
}
