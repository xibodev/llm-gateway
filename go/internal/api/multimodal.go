package api

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
