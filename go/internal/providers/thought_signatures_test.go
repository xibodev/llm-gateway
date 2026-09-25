package providers

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/xibodev/llm-translate"
)

func TestRejectMaterialLossExceptThoughtSignaturesIgnoresOnlyTheDrop(t *testing.T) {
	dropped := func(path string) translate.Loss {
		return translate.Loss{Path: path, Class: translate.LossDropped, Severity: translate.LossMaterial}
	}
	ignored := []translate.Loss{
		dropped("messages.1.tool_calls.0.extra_content.google.thought_signature"),
		dropped("choices.0.message.tool_calls.2.function.thought_signature"),
		dropped("choices.0.delta.tool_calls.1.thought_signature"),
	}
	if err := RejectMaterialLossExceptThoughtSignatures(translate.NewReport(ignored...)); err != nil {
		t.Fatalf("dropped thought signatures were rejected: %v", err)
	}
	unsupported := dropped("messages.1.tool_calls.0.thought_signature")
	unsupported.Class = translate.LossUnsupported
	for _, kept := range []translate.Loss{
		unsupported,
		dropped("thought_signature"),
		dropped("messages.1.thought_signature"),
		dropped("messages.1.tool_calls.0.extra_content.thought_signature"),
		dropped("messages.1.tool_calls.0.thought_signature.value"),
	} {
		report := translate.NewReport(append([]translate.Loss{kept}, ignored...)...)
		err := RejectMaterialLossExceptThoughtSignatures(report)
		want := translate.RejectMaterialLoss(translate.NewReport(kept))
		if err == nil || err.Error() != want.Error() {
			t.Errorf("%s %s: err=%v, want %v", kept.Class, kept.Path, err, want)
		}
	}
}

func TestRejectMaterialLossExceptThoughtSignaturesKeepsOtherConversionLosses(t *testing.T) {
	messages := []map[string]any{
		{"role": "narrator", "content": "hi"},
		{"role": "assistant", "content": nil, "tool_calls": []any{signedToolCall()}},
	}
	err := RejectMaterialLossExceptThoughtSignatures(translate.OpenAIMessagesToAnthropicWithReport(messages).Report)
	if err == nil || err.Error() != "conversion has material compatibility loss at messages.0.role" {
		t.Fatalf("err=%v, want only the role loss", err)
	}
}

func signedToolCall() map[string]any {
	return map[string]any{
		"id": "call_fixture", "type": "function", "thought_signature": "fixture-signature",
		"function":      map[string]any{"name": "lookup", "arguments": "{}", "thought_signature": "fixture-signature"},
		"extra_content": map[string]any{"google": map[string]any{"thought_signature": "fixture-signature"}, "other": "kept"},
	}
}

func TestWithoutThoughtSignaturesLeavesTheCodexRequestAndHistoryUnchanged(t *testing.T) {
	plain := map[string]any{"id": "call_plain", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}}
	messages := []Message{
		{"role": "user", "content": "Look it up"},
		{"role": "assistant", "content": nil, "tool_calls": []any{signedToolCall(), plain}},
		{"role": "tool", "tool_call_id": "call_fixture", "content": "found"},
		{"role": "tool", "tool_call_id": "call_plain", "content": "found"},
	}
	before, _ := json.Marshal(messages)
	stripped := withoutThoughtSignatures(messages)
	if after, _ := json.Marshal(messages); string(after) != string(before) {
		t.Fatalf("the caller's history changed: %s", after)
	}
	if encoded, _ := json.Marshal(stripped); strings.Contains(string(encoded), "thought_signature") ||
		!strings.Contains(string(encoded), `"other":"kept"`) {
		t.Fatalf("stripped=%s", encoded)
	}
	if !reflect.DeepEqual(translate.ChatToResponses("gpt-codex", stripped, nil, true), translate.ChatToResponses("gpt-codex", messages, nil, true)) {
		t.Fatal("removing the signatures changed the Responses request")
	}
	if report := translate.ChatToResponsesWithReport("gpt-codex", stripped, nil, true).Report; report.HasMaterialLoss() {
		t.Fatalf("report=%+v", report)
	}
	unsigned := []Message{{"role": "assistant", "content": nil, "tool_calls": []any{plain}}}
	if got := withoutThoughtSignatures(unsigned); &got[0] != &unsigned[0] {
		t.Fatal("a history without signatures was copied")
	}
}
