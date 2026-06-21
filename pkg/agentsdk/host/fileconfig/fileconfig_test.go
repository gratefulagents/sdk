package fileconfig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestSourceLoadsCRDShapedModeAndRoles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "modes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	modeYAML := `apiVersion: platform.gratefulagents.dev/v1alpha1
kind: ModeTemplate
metadata:
  name: feature-builder
spec:
  name: feature-builder
  version: v1
  displayName: Feature Builder
  description: Build a feature.
  category: direct
  autonomous: false
  modelRouting:
    defaultModel: openai/gpt-5.5
    fallbackModels:
      - anthropic/claude-sonnet-4-6
    reasoningLevel: xhigh
    textVerbosity: low
    roleOverrides:
      planner:
        model: openai/gpt-5.4
        fallbackModels:
          - copilot/gpt-4.1
        reasoningLevel: high
  phases:
    - id: scoping
      readOnly: true
    - id: implementing
      requiresApproval: true
      entryGates:
        - require: plan_exists
          message: Save a plan first.
  constraints:
    maxTurns: 120
    subAgentMaxTurns: 30
    maxConcurrentSubAgents: 3
  resetTo:
    mode: chat
    requiresApproval: true
    clearHistory: true
  instructions: |
    Follow the feature workflow.
`
	if err := os.WriteFile(filepath.Join(root, "modes", "feature-builder.yaml"), []byte(modeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	role := `---
name: planner
description: Writes the plan.
tool_access: analysis
model_override: openai/gpt-5.3-codex
---
Plan carefully.
`
	if err := os.WriteFile(filepath.Join(root, "agents", "planner.md"), []byte(role), 0o644); err != nil {
		t.Fatal(err)
	}

	src := New(root, t.TempDir(), WithActiveMode("feature-builder"), WithActivePhase("scoping"))
	ctx := context.Background()
	modes, err := src.ListModes(ctx)
	if err != nil {
		t.Fatalf("ListModes: %v", err)
	}
	if len(modes) != 1 || modes[0].DisplayName != "Feature Builder" {
		t.Fatalf("modes = %+v", modes)
	}
	snapshot, err := src.ModeSnapshot(ctx)
	if err != nil {
		t.Fatalf("ModeSnapshot: %v", err)
	}
	if snapshot == nil || snapshot.ModelRouting == nil || snapshot.ModelRouting.RoleOverrides["planner"].ReasoningLevel != "high" {
		t.Fatalf("snapshot routing = %+v", snapshot)
	}
	if len(snapshot.ModelRouting.FallbackModels) != 1 || snapshot.ModelRouting.FallbackModels[0] != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("snapshot default fallbacks = %#v", snapshot.ModelRouting.FallbackModels)
	}
	if got := snapshot.ModelRouting.RoleOverrides["planner"].FallbackModels; len(got) != 1 || got[0] != "copilot/gpt-4.1" {
		t.Fatalf("planner fallbacks = %#v", got)
	}
	if snapshot.Constraints == nil || snapshot.Constraints.MaxTurns != 120 || snapshot.ResetTo == nil || snapshot.ResetTo.Name != "chat" {
		t.Fatalf("snapshot constraints/reset = %+v", snapshot)
	}
	mode, err := src.PermissionMode(ctx)
	if err != nil {
		t.Fatalf("PermissionMode: %v", err)
	}
	if mode != agentsdk.PermissionModeReadOnly {
		t.Fatalf("PermissionMode = %q, want read-only", mode)
	}
	directive, err := src.PhaseDirective(ctx)
	if err != nil {
		t.Fatalf("PhaseDirective: %v", err)
	}
	for _, want := range []string{"Mode: Feature Builder", "Active phase: scoping", "Follow the feature workflow."} {
		if !strings.Contains(directive, want) {
			t.Fatalf("directive missing %q:\n%s", want, directive)
		}
	}
	catalog, err := src.RoleCatalog(ctx)
	if err != nil {
		t.Fatalf("RoleCatalog: %v", err)
	}
	if len(catalog) != 1 || catalog[0].Name != "planner" || catalog[0].ToolAccess != "read-only" || catalog[0].ModelOverride == "" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestGetModeRejectsPathTraversalName(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "modes"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "secret.yaml")
	if err := os.WriteFile(outside, []byte("name: secret\ninstructions: nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := New(root, t.TempDir()).GetMode(context.Background(), "../secret")
	if err == nil || !strings.Contains(err.Error(), "must be a single file name") {
		t.Fatalf("GetMode() error = %v, want traversal rejection", err)
	}
}
