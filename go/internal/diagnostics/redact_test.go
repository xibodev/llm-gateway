package diagnostics

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTextIsIdempotent(t *testing.T) {
	raw := `Authorization: Bearer small/value owner@example.test sk-abcdefghijklmnop`
	once := SanitizeText(raw)
	if strings.Contains(once, "small/value") || strings.Contains(once, "owner@example.test") || strings.Contains(once, "sk-") {
		t.Fatalf("sensitive text survived: %q", once)
	}
	if twice := SanitizeText(once); twice != once {
		t.Fatalf("sanitization is not idempotent: once=%q twice=%q", once, twice)
	}
}

func TestSanitizeTextLimitSanitizesBeforeRuneLimit(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("a", 32)
	got := SanitizeTextLimit(strings.Repeat("界", 190)+secret, 200)
	if utf8.RuneCountInString(got) > 200 {
		t.Fatalf("bounded text has %d runes", utf8.RuneCountInString(got))
	}
	if strings.Contains(got, secret[:24]) || !strings.Contains(got, Redacted) {
		t.Fatalf("credential was not sanitized before limiting: %q", got)
	}
	if got := SanitizeTextLimit("safe", 0); got != "" {
		t.Fatalf("zero limit returned %q", got)
	}
}

func TestSanitizeIdentifierPreservesFunctionalValuesAndRedactsExplicitSecrets(t *testing.T) {
	safe := []string{
		"en-US-EmmaMultilingualNeural",
		"anthropic/model-ThisIdentifierSegmentIsLongEnough",
		"provider-ThisProviderIdentifierSegmentIsVeryLong",
	}
	for _, value := range safe {
		if got := SanitizeIdentifier(value); got != value {
			t.Errorf("SanitizeIdentifier(%q) = %q", value, got)
		}
	}
	secret := "llmgw_" + strings.Repeat("a", 32)
	for _, value := range []string{
		secret,
		"gsk_abcdefghijklmnop",
		"ghp_abcdefghijklmnop",
		"sk-abcdefghijklmnop",
		"Bearer compact-token",
		"https://example.test/path?token=query-secret",
		"owner@example.test",
	} {
		if got := SanitizeIdentifier(value); strings.Contains(got, value) || !strings.Contains(got, Redacted) {
			t.Errorf("explicit secret survived: input=%q got=%q", value, got)
		}
	}
	if got := SanitizeIdentifierLimit(strings.Repeat("界", 30), 20); utf8.RuneCountInString(got) != 20 || !utf8.ValidString(got) {
		t.Fatalf("identifier limit is not rune-safe: %q", got)
	}
}

func TestSanitizeStructuredValueUsesFieldAwareStringPolicy(t *testing.T) {
	model := "anthropic/model-ThisIdentifierSegmentIsLongEnough"
	voice := "en-US-EmmaMultilingualNeural"
	provider := "provider-ThisProviderIdentifierSegmentIsVeryLong"
	secret := "llmgw_" + strings.Repeat("b", 32)
	input := map[string]any{
		"model":        model,
		"served_model": model,
		"nested": map[string]any{
			"voice":         voice,
			"provider_id":   provider,
			"model_ids":     []any{model, "owner@example.test"},
			"route":         []any{model, "gsk_abcdefghijklmnop"},
			"message":       model,
			"model_error":   model,
			"model_message": model,
			"unknown":       provider,
		},
		"endpoint":  secret,
		"operation": "owner@example.test",
		"token":     model,
	}
	got := SanitizeStructuredValue(input).(map[string]any)
	nested := got["nested"].(map[string]any)
	if got["model"] != model || got["served_model"] != model ||
		nested["voice"] != voice || nested["provider_id"] != provider {
		t.Fatalf("functional identifiers changed: %#v", got)
	}
	route := nested["route"].([]any)
	modelIDs := nested["model_ids"].([]any)
	if route[0] != model || route[1] != Redacted || got["endpoint"] != Redacted ||
		modelIDs[0] != model || modelIDs[1] != Redacted ||
		got["operation"] != Redacted || got["token"] != Redacted {
		t.Fatalf("explicit credentials survived functional fields: %#v", got)
	}
	if !strings.Contains(nested["message"].(string), Redacted) ||
		!strings.Contains(nested["model_error"].(string), Redacted) ||
		!strings.Contains(nested["model_message"].(string), Redacted) ||
		!strings.Contains(nested["unknown"].(string), Redacted) ||
		nested["message"] == model || nested["unknown"] == provider {
		t.Fatalf("free text did not retain full sanitation: %#v", nested)
	}
	if input["model"] != model || input["endpoint"] != secret ||
		input["nested"].(map[string]any)["voice"] != voice {
		t.Fatalf("input was mutated: %#v", input)
	}
	if twice := SanitizeStructuredValue(got); !reflect.DeepEqual(twice, got) {
		t.Fatalf("field-aware sanitation is not idempotent: once=%#v twice=%#v", got, twice)
	}
}

func TestSanitizeValueNestedCopyAndScalars(t *testing.T) {
	input := map[string]any{
		"authorization": "short-value",
		"detail":        "owner@example.test",
		"count":         3,
		"enabled":       true,
		"empty":         nil,
		"nested": []any{
			map[string]any{"api_key": "tiny", "status": 429},
			"Bearer compact-token",
		},
		"credential": []any{"short", 7, map[string]any{"label": "also-short"}},
	}
	want := map[string]any{
		"authorization": Redacted,
		"detail":        Redacted,
		"count":         3,
		"enabled":       true,
		"empty":         nil,
		"nested": []any{
			map[string]any{"api_key": Redacted, "status": 429},
			"Bearer " + Redacted,
		},
		"credential": []any{Redacted, 7, map[string]any{"label": Redacted}},
	}

	got := SanitizeValue(input)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SanitizeValue() = %#v, want %#v", got, want)
	}
	if input["authorization"] != "short-value" || input["detail"] != "owner@example.test" {
		t.Fatalf("input map was mutated: %#v", input)
	}
	nested := input["nested"].([]any)
	if nested[0].(map[string]any)["api_key"] != "tiny" || nested[1] != "Bearer compact-token" {
		t.Fatalf("nested input was mutated: %#v", nested)
	}
	if twice := SanitizeValue(got); !reflect.DeepEqual(twice, got) {
		t.Fatalf("structured sanitization is not idempotent: once=%#v twice=%#v", got, twice)
	}
}

func TestSanitizeValueSupportsNamedContainers(t *testing.T) {
	type namedMap map[string]any
	type namedSlice []any
	input := namedMap{"items": namedSlice{"owner@example.test", namedMap{"password": "short"}}}
	want := map[string]any{"items": []any{Redacted, map[string]any{"password": Redacted}}}
	if got := SanitizeValue(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("SanitizeValue() = %#v, want %#v", got, want)
	}
}

func TestSanitizeValuePreservesScalarPrimitives(t *testing.T) {
	for _, value := range []any{nil, true, int64(-2), uint32(4), float64(1.5)} {
		if got := SanitizeValue(value); !reflect.DeepEqual(got, value) {
			t.Errorf("SanitizeValue(%#v) = %#v", value, got)
		}
	}
	if got := SanitizeValue("owner@example.test"); got != Redacted {
		t.Fatalf("string scalar was not sanitized: %#v", got)
	}
}

func TestSanitizeValueBoundsDepthCollectionsAndCycles(t *testing.T) {
	cycle := map[string]any{"safe": "value"}
	cycle["self"] = cycle
	got := SanitizeValue(cycle).(map[string]any)
	if got["self"] != Redacted {
		t.Fatalf("cycle was not bounded: %#v", got)
	}

	const safe = "safe"
	within := any(safe)
	for range maxStructuredDepth {
		within = []any{within}
	}
	gotWithin := SanitizeValue(within)
	for depth := range maxStructuredDepth {
		items, ok := gotWithin.([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("within-bound value at depth %d = %#v, want one-element slice", depth, gotWithin)
		}
		gotWithin = items[0]
	}
	if gotWithin != safe {
		t.Fatalf("within-bound terminal value = %#v, want %q", gotWithin, safe)
	}

	beyond := any(safe)
	for range maxStructuredDepth + 1 {
		beyond = []any{beyond}
	}
	gotBeyond := SanitizeValue(beyond)
	for depth := range maxStructuredDepth {
		items, ok := gotBeyond.([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("beyond-bound value at depth %d = %#v, want one-element slice", depth, gotBeyond)
		}
		gotBeyond = items[0]
	}
	if gotBeyond != Redacted {
		t.Fatalf("beyond-bound terminal value = %#v, want %q", gotBeyond, Redacted)
	}

	oversized := make([]any, maxStructuredElements+1)
	if got := SanitizeValue(oversized); got != Redacted {
		t.Fatalf("oversized collection was not bounded: %#v", got)
	}

	wide := make([]any, maxStructuredElements)
	wide[0] = []any{"owner@example.test"}
	if got := SanitizeValue(wide).([]any)[0]; got != Redacted {
		t.Fatalf("global collection budget was not enforced: %#v", got)
	}
}
