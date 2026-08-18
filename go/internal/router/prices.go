package router

import "strings"

// priceEntry is USD per 1 million tokens (input, output).
type priceEntry struct{ in, out float64 }

// A small built-in price catalog keyed by model-name substring. Overrides from
// settings.savings.price_catalog take precedence. Unknown models cost 0.
var defaultPrices = map[string]priceEntry{
	"claude-opus":    {15.0, 75.0},
	"claude-sonnet":  {3.0, 15.0},
	"claude-haiku":   {0.80, 4.0},
	"gpt-4o-mini":    {0.15, 0.60},
	"gpt-4o":         {2.50, 10.0},
	"gpt-4.1":        {2.0, 8.0},
	"gpt-4":          {30.0, 60.0},
	"gpt-3.5":        {0.50, 1.50},
	"gpt-5":          {1.25, 10.0},
	"o3":             {2.0, 8.0},
	"gemini-2.5-pro": {1.25, 10.0},
	"gemini-3":       {2.0, 12.0},
	"gemini":         {0.50, 1.50},
	"llama":          {0.20, 0.20},
	"gemma":          {0.10, 0.10},
	"deepseek":       {0.27, 1.10},
	"mistral":        {0.40, 2.0},
}

func lookupPrice(model string, overrides map[string]map[string]float64) (float64, float64, bool) {
	if model == "" {
		return 0, 0, false
	}
	if overrides != nil {
		if p, ok := overrides[model]; ok {
			return p["input"], p["output"], true
		}
	}
	low := strings.ToLower(model)
	// longest-substring-first for specificity
	var bestKey string
	for key := range defaultPrices {
		if strings.Contains(low, key) && len(key) > len(bestKey) {
			bestKey = key
		}
	}
	if bestKey != "" {
		p := defaultPrices[bestKey]
		return p.in, p.out, true
	}
	return 0, 0, false
}

// computeCost returns USD for a call given token counts.
func computeCost(model string, inputTokens, outputTokens int, overrides map[string]map[string]float64) float64 {
	in, out, ok := lookupPrice(model, overrides)
	if !ok {
		return 0
	}
	return (float64(inputTokens)*in + float64(outputTokens)*out) / 1_000_000.0
}
