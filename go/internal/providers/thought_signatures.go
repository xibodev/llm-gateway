package providers

import (
	"regexp"
	"slices"

	"github.com/xibodev/llm-translate"
)

// llm-translate v0.3.0 reports a dropped Gemini thought signature as a
// material loss, because Gemini needs the signature back on the next turn of
// a tool call. Earlier versions dropped it silently: neither Anthropic
// Messages nor Responses has a place for it. The gateway is stateless and the
// client keeps its own history, so rejecting the drop would break failover
// from a Gemini target to any other, and serving Anthropic Messages and
// Responses clients from Gemini or Antigravity models. The gateway keeps
// accepting the drop until it adopts a LossPolicy in a later milestone, where
// the decision belongs; this file then goes away.

// thoughtSignaturePath matches where a dropped signature is reported: the
// extra_content.google, function or call-level thought_signature of a tool
// call in a message, a response choice or a stream delta.
var thoughtSignaturePath = regexp.MustCompile(`(^|\.)tool_calls\.\d+\.(extra_content\.google\.|function\.)?thought_signature$`)

// RejectMaterialLossExceptThoughtSignatures rejects the material losses of a
// report as translate.RejectMaterialLoss does, ignoring dropped thought
// signatures. The error names the paths it named before v0.3.0. Only the
// drop is ignored: any other class of loss at such a path still rejects.
func RejectMaterialLossExceptThoughtSignatures(report translate.Report) error {
	kept := make([]translate.Loss, 0, len(report.Losses))
	for _, loss := range report.Losses {
		if loss.Class != translate.LossDropped || !thoughtSignaturePath.MatchString(loss.Path) {
			kept = append(kept, loss)
		}
	}
	return translate.RejectMaterialLoss(translate.Report{Losses: kept})
}

// withoutThoughtSignatures removes the thought signatures from the tool calls
// of messages bound for the shared Codex provider. That provider rejects
// every material loss of its own Chat-to-Responses conversion, where the
// gateway cannot filter, and Responses has no place for a signature, so the
// request Codex receives is the same either way. Changed messages and calls
// are copies: failover replays the caller's history to the next target,
// which may need the signatures.
func withoutThoughtSignatures(messages []Message) []Message {
	var out []Message
	for i, message := range messages {
		calls, _ := message["tool_calls"].([]any)
		var cleaned []any
		for j, raw := range calls {
			call, _ := raw.(map[string]any)
			if clean, changed := withoutThoughtSignature(call); changed {
				if cleaned == nil {
					cleaned = slices.Clone(calls)
				}
				cleaned[j] = clean
			}
		}
		if cleaned == nil {
			continue
		}
		if out == nil {
			out = slices.Clone(messages)
		}
		out[i] = cloneMap(message)
		out[i]["tool_calls"] = cleaned
	}
	if out == nil {
		return messages
	}
	return out
}

// withoutThoughtSignature removes the signature from each place a provider
// stores one on a tool call, and reports whether the call carried any.
func withoutThoughtSignature(call map[string]any) (map[string]any, bool) {
	function, _ := call["function"].(map[string]any)
	extra, _ := call["extra_content"].(map[string]any)
	google, _ := extra["google"].(map[string]any)
	_, atCall := call["thought_signature"]
	_, atFunction := function["thought_signature"]
	_, atGoogle := google["thought_signature"]
	if !atCall && !atFunction && !atGoogle {
		return call, false
	}
	out := cloneMap(call)
	delete(out, "thought_signature")
	if atFunction {
		function = cloneMap(function)
		delete(function, "thought_signature")
		out["function"] = function
	}
	if atGoogle {
		google = cloneMap(google)
		delete(google, "thought_signature")
		extra = cloneMap(extra)
		extra["google"] = google
		out["extra_content"] = extra
	}
	return out, true
}
