package anthropic

import "testing"

// TestModelThinkingShapeCapabilities pins the per-generation thinking-shape
// split mirrored from the models.dev reasoning_options catalogs: the 4.5
// generation and older only implement enabled+budget_tokens, 4.6+/fable/5.x
// implement adaptive+output_config.effort, and only the effort-only models
// (fable, opus 4.7+, generation 5) reject enabled on the first-party API.
func TestModelThinkingShapeCapabilities(t *testing.T) {
	cases := []struct {
		model    string
		supports bool // ModelSupportsAdaptiveThinking
		requires bool // ModelRequiresAdaptiveThinking
	}{
		// Budget-only generation (Copilot's /v1/messages shim returns no
		// thinking blocks for adaptive requests against these).
		{"claude-haiku-4.5", false, false},
		{"claude-haiku-4-5", false, false},
		{"claude-haiku-4-5-20251001", false, false},
		{"claude-sonnet-4.5", false, false},
		{"claude-sonnet-4-5-20250929", false, false},
		{"claude-opus-4.5", false, false},
		{"claude-opus-4-5-20251101", false, false},
		{"claude-sonnet-4", false, false},
		{"claude-sonnet-4-20250514", false, false},
		{"claude-opus-4-1", false, false},
		{"claude-3-7-sonnet", false, false},
		{"claude-3-5-haiku-20241022", false, false},

		// Adaptive-capable, budget still accepted on api.anthropic.com.
		{"claude-sonnet-4.6", true, false},
		{"claude-sonnet-4-6", true, false},
		{"claude-opus-4.6", true, false},
		{"claude-opus-4-6", true, false},

		// Effort-only models: adaptive everywhere.
		{"claude-opus-4.7", true, true},
		{"claude-opus-4-7", true, true},
		{"claude-opus-4.8", true, true},
		{"claude-sonnet-5", true, true},
		{"claude-fable-5", true, true},
		{"claude-fable-5-20260601", true, true},

		// Prefixed model IDs are tolerated.
		{"copilot/claude-sonnet-4.5", false, false},
		{"anthropic/claude-fable-5", true, true},

		// Non-Claude models never match.
		{"gpt-5.2", false, false},
		{"gemini-3-pro", false, false},
		{"", false, false},
	}
	for _, tc := range cases {
		if got := ModelSupportsAdaptiveThinking(tc.model); got != tc.supports {
			t.Errorf("ModelSupportsAdaptiveThinking(%q) = %v, want %v", tc.model, got, tc.supports)
		}
		if got := ModelRequiresAdaptiveThinking(tc.model); got != tc.requires {
			t.Errorf("ModelRequiresAdaptiveThinking(%q) = %v, want %v", tc.model, got, tc.requires)
		}
	}
}
