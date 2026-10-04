package providers

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
)

// ChatFieldsAsSent marks the kwargs of a Chat Completions request as its
// client sent it: every kwarg whose name does not begin with "_" is a field
// the client set, so an OpenAI-compatible instance forwards each of them
// (see openAICompatibleProvider.chat).
const ChatFieldsAsSent = "_chat_fields_as_sent"

// UnsentChatField returns a field of a client's Chat Completions request,
// kw, that providerID, configured as cfg, would not send upstream for
// model, and false when it sends every field that asks for something. A
// field that asks for nothing (see chatFieldAsksNothing) loses nothing when
// it is not sent. Names that begin with "_" are the gateway's own, and
// stream_options is honoured by the gateway's stream whatever the upstream
// does with it, so neither is weighed. A provider that is not configured or
// is disabled, and a type that sends no Chat request upstream, such as the
// echo stub or an audio type, make no claim here: the request reaches them
// as before.
//
// Whether adaptation serves the model over Responses is read from the
// cached catalog only, as capability filtering reads it, so weighing a
// route discovers no catalog; the facade refuses what a fresher row turns
// unsendable (see openAICompatibleProvider.chat).
func (rt *Runtime) UnsentChatField(cfg *config.ProviderConfig, providerID, model string, caller core.Caller, kw Kwargs) (string, bool) {
	if cfg == nil || cfg.Disabled {
		return "", false
	}
	sends := rt.chatFieldsSent(providerID, model, caller, cfg, kw)
	if sends == nil {
		return "", false
	}
	return unsentChatField(kw, sends)
}

// unsentChatField returns the first field of kw, in name order, that sends
// does not name and that asks for something.
func unsentChatField(kw Kwargs, sends func(string) bool) (string, bool) {
	for _, field := range slices.Sorted(maps.Keys(kw)) {
		if strings.HasPrefix(field, "_") || field == "stream_options" || sends(field) || chatFieldAsksNothing(field, kw[field]) {
			continue
		}
		return field, true
	}
	return "", false
}

// unsentChatFieldError refuses, before anything is sent, a client's Chat
// request whose provider would not send field. It reads as a 400 naming
// the field, and a route moves on to its next member.
func unsentChatFieldError(field string) error {
	return &InvocationError{
		Msg:    fmt.Sprintf("the provider cannot send the Chat Completions field %q", field),
		Status: http.StatusBadRequest, FailoverEligible: true,
	}
}

// chatFieldsSent reports the Chat fields the facade the factory builds for
// cfg sends upstream for model, or nil when it sends every one. It follows
// the factory's choice of facade, and each facade's own reading of the
// kwargs, which is where a field it does not name is dropped.
func (rt *Runtime) chatFieldsSent(providerID, model string, caller core.Caller, cfg *config.ProviderConfig, kw Kwargs) func(string) bool {
	ptype := strings.ToLower(strings.TrimSpace(cfg.Type))
	if strings.EqualFold(strings.TrimSpace(cfg.RegistryID), ExtensionTypeCodex) {
		ptype = ExtensionTypeCodex
	}
	switch {
	case ptype == ExtensionTypeCopilot:
		// The daemon serves Copilot on the OpenAI wire, with what the
		// gateway's Copilot transport sent.
		return openAITransportField
	case isExtensionType(ptype):
		return daemonChatField
	}
	switch ptype {
	case "openai_compatible", "openai", "litellm", "bedrock":
		if adaptEnabled(kw, cfg.ForceApiSupport) && rt.cachedOverResponses(providerID, model, caller) {
			return responsesAdaptedChatField
		}
		if ptype == "bedrock" {
			return openAITransportField
		}
		return nil
	case "azure_openai":
		return openAITransportField
	case "anthropic":
		return anthropicChatField
	case "ai_studio", "vertex_ai":
		return googleChatField
	case "ollama":
		return ollamaChatField
	}
	return nil
}

// cachedOverResponses reports a model of an OpenAI-wire instance whose
// cached catalog row lists Responses but not Chat Completions, so that
// adaptation serves its Chat over Responses.
func (rt *Runtime) cachedOverResponses(instance, model string, caller core.Caller) bool {
	row, ok := rt.CatalogCachedLookupForPrincipal(instance, model, caller)
	return ok && translate.PreferredEndpoint(row.SupportedSurfaces) == "responses"
}

// daemonChatField reports a Chat field the gateway hands the companion
// daemon for a type other than Copilot. What the daemon sends on is not
// visible here, so only the fields the Chat API has always handed it count
// as sent.
func daemonChatField(field string) bool {
	return field != "parallel_tool_calls" && field != "thinking" && openAITransportField(field)
}

// responsesAdaptedChatField reports a Chat field adaptation carries over
// Responses: those core converts, less stop, which the conversion refuses.
func responsesAdaptedChatField(field string) bool {
	switch field {
	case "temperature", "top_p", "max_tokens", "max_completion_tokens", "tools", "tool_choice", "metadata", "reasoning_effort":
		return true
	}
	return false
}

// anthropicChatField reports a Chat field the Anthropic facade carries into
// its Messages request; see AnthropicNativeProvider.payload.
func anthropicChatField(field string) bool {
	switch field {
	case "temperature", "top_p", "max_tokens", "stop", "tools", "metadata", "thinking", "output_config":
		return true
	}
	return false
}

// googleChatField reports a Chat field the Google facade carries into
// generateContent; see googleProvider.CompleteContext.
func googleChatField(field string) bool {
	return field == "max_tokens" || field == "temperature"
}

// ollamaChatField reports a Chat field core's Ollama carries into Ollama's
// /api/chat.
func ollamaChatField(field string) bool {
	switch field {
	case "temperature", "max_tokens", "top_p", "tools":
		return true
	}
	return false
}

// chatFieldAsksNothing reports a value that asks for what a provider does
// anyway, as core reads one when it drops a field: null, false, an empty
// string, list or object, one choice, a zero penalty, text-only output, a
// text response format, an automatic tool choice and parallel tool calls.
// Zero asks for nothing only where it is the value that turns a feature off,
// so a seed of 0 still asks for a seed, and an empty web_search_options turns
// web search on.
func chatFieldAsksNothing(field string, value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case bool:
		return typed == (field == "parallel_tool_calls")
	case float64:
		switch field {
		case "n":
			return typed == 1
		case "presence_penalty", "frequency_penalty", "top_logprobs":
			return typed == 0
		}
	case string:
		return typed == "" || (field == "tool_choice" && typed == "auto")
	case []any:
		return len(typed) == 0 || (field == "modalities" && len(typed) == 1 && typed[0] == "text")
	case map[string]any:
		if field == "response_format" {
			return typed["type"] == "text"
		}
		return len(typed) == 0 && field != "web_search_options"
	}
	return false
}
