package providers

import (
	"regexp"

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
// the decision belongs; this file then goes away. Codex needs nothing here:
// llmgw-core converts Codex requests itself and accepts the drop there.

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
