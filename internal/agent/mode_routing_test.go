package agent

import "testing"

func TestModeReasoningSettings(t *testing.T) {
	tests := []struct {
		name       string
		level      ModeReasoningLevel
		wantBudget int
		wantEffort string
	}{
		{name: "none", level: ReasoningNone, wantEffort: "minimal"},
		{name: "low", level: ReasoningLow, wantBudget: 2048, wantEffort: "low"},
		{name: "medium", level: ReasoningMedium, wantBudget: 4096, wantEffort: "medium"},
		{name: "high", level: ReasoningHigh, wantBudget: 8192, wantEffort: "high"},
		{name: "xhigh", level: ReasoningXHigh, wantBudget: 12288, wantEffort: "xhigh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ModeReasoningSettings(tt.level)
			if got.ThinkingBudget != tt.wantBudget || got.ReasoningEffort != tt.wantEffort {
				t.Fatalf("ModeReasoningSettings(%q) = budget=%d effort=%q, want budget=%d effort=%q",
					tt.level, got.ThinkingBudget, got.ReasoningEffort, tt.wantBudget, tt.wantEffort)
			}
		})
	}
}

func TestResolveRoleModeRouting(t *testing.T) {
	routing := &ModeModelRouting{
		DefaultModel:   "openai/gpt-5.4",
		ReasoningLevel: string(ReasoningMedium),
		TextVerbosity:  string(TextVerbosityLow),
		RoleOverrides: map[string]ModeRoleModelRouting{
			"architect": {
				Model:          "anthropic/claude-sonnet-4-6",
				ReasoningLevel: string(ReasoningHigh),
				TextVerbosity:  string(TextVerbosityHigh),
			},
		},
	}

	architect := ResolveRoleModeRouting("openai/gpt-5.3-codex", "openai", "architect", routing)
	if architect.Model != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("architect model = %q, want anthropic/claude-sonnet-4-6", architect.Model)
	}
	if architect.ReasoningLevel != "high" || architect.ModelSettings.ThinkingBudget != 8192 {
		t.Fatalf("architect reasoning = %q budget=%d", architect.ReasoningLevel, architect.ModelSettings.ThinkingBudget)
	}
	if architect.TextVerbosity != "high" || architect.ModelSettings.TextVerbosity != "high" {
		t.Fatalf("architect verbosity = %q settings=%q, want high", architect.TextVerbosity, architect.ModelSettings.TextVerbosity)
	}

	planner := ResolveRoleModeRouting("openai/gpt-5.3-codex", "openai", "planner", routing)
	if planner.Model != "openai/gpt-5.4" {
		t.Fatalf("planner model = %q, want openai/gpt-5.4", planner.Model)
	}
	if planner.ReasoningLevel != "medium" || planner.ModelSettings.ThinkingBudget != 4096 {
		t.Fatalf("planner reasoning = %q budget=%d", planner.ReasoningLevel, planner.ModelSettings.ThinkingBudget)
	}
	if planner.TextVerbosity != "low" || planner.ModelSettings.TextVerbosity != "low" {
		t.Fatalf("planner verbosity = %q settings=%q, want low", planner.TextVerbosity, planner.ModelSettings.TextVerbosity)
	}

	noModeReasoning := ResolveRoleModeRouting("openai/gpt-5.3-codex", "openai", "executor", &ModeModelRouting{})
	if noModeReasoning.ReasoningLevel != "" || noModeReasoning.ModelSettings.ThinkingBudget != 0 {
		t.Fatalf("no mode reasoning = %q budget=%d", noModeReasoning.ReasoningLevel, noModeReasoning.ModelSettings.ThinkingBudget)
	}
}
