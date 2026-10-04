package router

import (
	"fmt"

	"llmgw/internal/config"
	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
)

// FilterChatFieldTargets removes the targets whose provider, as settings
// configures it, would not send a field of a client's Chat Completions
// request, kw, upstream: such a target cannot serve the request as it was
// sent, so it is excluded as FilterCompatibleTargets excludes one that
// cannot serve it at all, and another member of the route serves. When none
// remains, the error names the field that excluded the first.
func (rt *Runtime) FilterChatFieldTargets(targets []Target, settings *config.Settings, caller core.Caller, kw providers.Kwargs) ([]Target, error) {
	kept := make([]Target, 0, len(targets))
	unsent := ""
	for _, target := range targets {
		field, ok := rt.providers().UnsentChatField(settings.Providers[target.Provider], target.Provider, target.Model, caller, kw)
		if ok {
			if unsent == "" {
				unsent = field
			}
			continue
		}
		kept = append(kept, target)
	}
	if len(kept) == 0 && unsent != "" {
		return nil, &providers.ConfigError{Msg: fmt.Sprintf("no route member can send the Chat Completions field %q", unsent)}
	}
	return kept, nil
}
