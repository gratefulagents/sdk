package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

type adaptingTestTool struct {
	name     string
	readOnly bool
}

func (t adaptingTestTool) Name() string                 { return t.name }
func (t adaptingTestTool) Description() string          { return "" }
func (t adaptingTestTool) InputSchema() json.RawMessage { return nil }
func (t adaptingTestTool) Execute(context.Context, json.RawMessage, string) (ToolResult, error) {
	return ToolResult{}, nil
}
func (t adaptingTestTool) IsReadOnly() bool             { return t.readOnly }
func (t adaptingTestTool) IsEnabled(_ *RunContext) bool { return true }
func (t adaptingTestTool) NeedsApproval() bool          { return false }
func (t adaptingTestTool) TimeoutSeconds() int          { return 0 }
func (t adaptingTestTool) ToolForAccess(ToolAccessLevel) Tool {
	return adaptingTestTool{name: t.name, readOnly: true}
}

func TestFilterToolsByAccessUsesAdapterForReadOnly(t *testing.T) {
	filtered := FilterToolsByAccess([]Tool{
		&FunctionTool{ToolName: "Read", ReadOnly: true},
		&FunctionTool{ToolName: "Edit", ReadOnly: false},
		adaptingTestTool{name: "Bash", readOnly: false},
	}, "read-only")
	if len(filtered) != 2 {
		t.Fatalf("len(filtered) = %d, want 2", len(filtered))
	}
	if filtered[1].Name() != "Bash" || !filtered[1].IsReadOnly() {
		t.Fatalf("adapted tool = %s readOnly=%v, want read-only Bash", filtered[1].Name(), filtered[1].IsReadOnly())
	}
}

func TestBuildDelegationGuideIncludesCompactTaskPacketGuidance(t *testing.T) {
	a := &Agent{Tools: []Tool{&FunctionTool{ToolName: "agent_executor", ToolDescription: "Implement a bounded change"}}}
	guide := BuildDelegationGuide(a)
	for _, want := range []string{
		"compact, self-contained task packet",
		"Do NOT send the same large background block to every task if only one sub-agent needs it.",
		"Files you own",
	} {
		if !strings.Contains(guide, want) {
			t.Fatalf("BuildDelegationGuide() missing %q\n%s", want, guide)
		}
	}
}

func TestFilterRoleCatalogForMode(t *testing.T) {
	catalog := RoleCatalog{
		{Name: "researcher"},
		{Name: "executor"},
	}
	filtered := FilterRoleCatalogForMode(catalog, &sdkmode.TemplateSpec{Capabilities: []string{"researcher"}})
	if len(filtered) != 1 || filtered[0].Name != "researcher" {
		t.Fatalf("filtered = %#v, want researcher only", filtered)
	}
}

func TestBuildSpecialistToolsFromCatalogAppliesAccessAndRouting(t *testing.T) {
	runner := NewRunnerWithProvider(stubModelProvider{})
	writeTool := &FunctionTool{ToolName: "Write", ReadOnly: false}
	readTool := &FunctionTool{ToolName: "Read", ReadOnly: true}
	finishTool := &FunctionTool{ToolName: "finish", ReadOnly: false}

	result := BuildSpecialistToolsFromCatalog(RoleCatalog{{
		Name:         "researcher",
		Description:  "Research role",
		Instructions: "Research carefully.",
		ToolAccess:   "analysis",
	}}, SpecialistBuildOptions{
		Runner:    runner,
		Tools:     []Tool{writeTool, readTool, finishTool},
		BaseModel: "base-model",
		Provider:  "openai",
		ModeSnapshot: &sdkmode.TemplateSpec{ModelRouting: &sdkmode.ModelRouting{
			FallbackModels: []string{"anthropic/claude-sonnet-4-6"},
			RoleOverrides: map[string]sdkmode.RoleModelRouting{
				"researcher": {Model: "role-model", FallbackModels: []string{"copilot/gpt-4.1"}},
			},
		}},
	})

	agent := result.Agents["researcher"]
	if agent == nil {
		t.Fatal("researcher agent missing")
	}
	if agent.Model != "role-model" {
		t.Fatalf("agent.Model = %q, want role-model", agent.Model)
	}
	if len(agent.FallbackModels) != 1 || agent.FallbackModels[0] != "copilot/gpt-4.1" {
		t.Fatalf("agent.FallbackModels = %#v", agent.FallbackModels)
	}
	if len(agent.Tools) != 1 || agent.Tools[0].Name() != "Read" {
		t.Fatalf("agent.Tools = %#v, want only read tool", agent.Tools)
	}
	if len(result.Tools) != 1 || result.Tools[0].Name() != "agent_researcher" {
		t.Fatalf("result.Tools = %#v, want agent_researcher", result.Tools)
	}
}

type stubModelProvider struct{}

func (stubModelProvider) GetModel(name string) (Model, error) { return stubModel{name: name}, nil }
func (stubModelProvider) Close() error                        { return nil }

type stubModel struct{ name string }

func (m stubModel) GetResponse(context.Context, ModelRequest) (*ModelResponse, error) {
	return &ModelResponse{}, nil
}

func (m stubModel) StreamResponse(context.Context, ModelRequest) (*ModelStream, error) {
	return nil, nil
}

func (m stubModel) GetRetryAdvice(error) *ModelRetryAdvice { return &ModelRetryAdvice{} }
func (m stubModel) CalculateCost(Usage) float64            { return 0 }
func (m stubModel) Provider() string                       { return "stub" }
