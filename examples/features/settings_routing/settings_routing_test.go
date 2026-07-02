package settings_routing_test

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestSettingsAndModeRoutingExample(t *testing.T) {
	routing := &agentsdk.ModeModelRouting{
		DefaultModel:   "medium",
		ReasoningLevel: string(agentsdk.ReasoningHigh),
		TextVerbosity:  string(agentsdk.TextVerbosityLow),
		RoleOverrides: map[string]agentsdk.ModeRoleModelRouting{
			"writer": {
				Model:          "small",
				ReasoningLevel: string(agentsdk.ReasoningLow),
				TextVerbosity:  string(agentsdk.TextVerbosityHigh),
			},
		},
	}

	top := agentsdk.ResolveModeRouting("", "anthropic", routing)
	if top.Model != "claude-sonnet-4-6" ||
		top.ModelSettings.ReasoningEffort != "high" ||
		top.ModelSettings.TextVerbosity != "low" {
		t.Fatalf("top routing = %+v", top)
	}

	writer := agentsdk.ResolveRoleModeRouting("", "anthropic", "writer", routing)
	if writer.Model != "claude-haiku-4-5" ||
		writer.ModelSettings.ReasoningEffort != "low" ||
		writer.ModelSettings.TextVerbosity != "high" {
		t.Fatalf("writer routing = %+v", writer)
	}
}
