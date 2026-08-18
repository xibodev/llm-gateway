package api

import (
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// requestIsMultimodal reports whether any message carries non-text content
// (image_url / input_image / file / input_audio, …) — i.e. it needs a
// vision/multimodal-capable model.
func requestIsMultimodal(messages []map[string]any) bool {
	for _, m := range messages {
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text", "input_text", nil, "":
				// textual part
			default:
				return true
			}
		}
	}
	return false
}

// targetHasVision consults the catalog for a target's vision capability.
func targetHasVision(t router.Target) bool {
	mi, ok := providers.CatalogLookup(t.Provider, t.Model)
	if !ok {
		return false
	}
	if mi.Capabilities == nil {
		return false
	}
	v, _ := mi.Capabilities["vision"].(bool)
	return v
}

// filterVisionTargets keeps only vision-capable targets. If the catalog knows of
// none but there are targets (unknown capabilities), it returns the originals so
// we don't block a provider that simply doesn't publish capabilities.
func filterVisionTargets(targets []router.Target) []router.Target {
	var vision, known []router.Target
	for _, t := range targets {
		if mi, ok := providers.CatalogLookup(t.Provider, t.Model); ok && len(mi.Capabilities) > 0 {
			known = append(known, t)
			if v, _ := mi.Capabilities["vision"].(bool); v {
				vision = append(vision, t)
			}
		}
	}
	if len(vision) > 0 {
		return vision
	}
	// No catalog capability info at all -> don't second-guess; let it through.
	if len(known) == 0 {
		return targets
	}
	// We had capability info and none were vision-capable.
	return nil
}
