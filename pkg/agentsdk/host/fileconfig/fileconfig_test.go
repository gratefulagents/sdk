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
  toolAccess: read-only
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
  constraints:
    maxTurns: 120
    subAgentMaxTurns: 30
    maxConcurrentSubAgents: 3
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

	src := New(root, t.TempDir(), WithActiveMode("feature-builder"))
	ctx := context.Background()
	modes, err := src.ListModes(ctx)
	if err != nil {
		t.Fatalf("ListModes: %v", err)
	}
	if len(modes) != 3 {
		t.Fatalf("modes = %d, want file mode plus builtin chat/plan: %+v", len(modes), modes)
	}
	byName := map[string]bool{}
	for _, m := range modes {
		byName[m.Name] = true
	}
	if !byName["feature-builder"] || !byName["chat"] || !byName["plan"] {
		t.Fatalf("modes missing expected names: %+v", modes)
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
	if snapshot.Constraints == nil || snapshot.Constraints.MaxTurns != 120 {
		t.Fatalf("snapshot constraints = %+v", snapshot)
	}
	if snapshot.ToolAccess != "read-only" {
		t.Fatalf("snapshot tool access = %q, want read-only", snapshot.ToolAccess)
	}
	mode, err := src.PermissionMode(ctx)
	if err != nil {
		t.Fatalf("PermissionMode: %v", err)
	}
	if mode != agentsdk.PermissionModeReadOnly {
		t.Fatalf("PermissionMode = %q, want read-only", mode)
	}
	directive, err := src.ModeDirective(ctx)
	if err != nil {
		t.Fatalf("ModeDirective: %v", err)
	}
	for _, want := range []string{"Mode: Feature Builder", "Tool access: read-only", "Follow the feature workflow."} {
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

func TestBuiltinChatAndPlanModes(t *testing.T) {
	root := t.TempDir() // no modes/ directory at all
	src := New(root, t.TempDir())
	ctx := context.Background()

	chat, err := src.GetMode(ctx, "chat")
	if err != nil {
		t.Fatalf("GetMode(chat): %v", err)
	}
	if chat.Name != "chat" || chat.ToolAccess != "" {
		t.Fatalf("builtin chat = %+v", chat)
	}

	plan, err := src.GetMode(ctx, "plan")
	if err != nil {
		t.Fatalf("GetMode(plan): %v", err)
	}
	if plan.Name != "plan" || plan.ToolAccess != "read-only" || plan.Instructions == "" {
		t.Fatalf("builtin plan = %+v", plan)
	}

	planSrc := New(root, t.TempDir(), WithActiveMode("plan"))
	mode, err := planSrc.PermissionMode(ctx)
	if err != nil {
		t.Fatalf("PermissionMode: %v", err)
	}
	if mode != agentsdk.PermissionModeReadOnly {
		t.Fatalf("plan PermissionMode = %q, want read-only", mode)
	}
}

func TestModeFileOverridesBuiltin(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "modes"), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `name: chat
displayName: Custom Chat
instructions: |
  Custom chat instructions.
`
	if err := os.WriteFile(filepath.Join(root, "modes", "chat.yaml"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}
	src := New(root, t.TempDir())
	ctx := context.Background()
	chat, err := src.GetMode(ctx, "chat")
	if err != nil {
		t.Fatalf("GetMode(chat): %v", err)
	}
	if chat.DisplayName != "Custom Chat" {
		t.Fatalf("file should override builtin chat: %+v", chat)
	}
	modes, err := src.ListModes(ctx)
	if err != nil {
		t.Fatalf("ListModes: %v", err)
	}
	count := 0
	for _, m := range modes {
		if strings.EqualFold(m.Name, "chat") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("chat listed %d times, want 1: %+v", count, modes)
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
