package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkmcp "github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/shell"
	sdkvision "github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

func TestProviderSpecDefaultsOpenAIToAPIKey(t *testing.T) {
	spec := ProviderSpec(Config{Provider: "openai"})
	if spec.AuthMode != "api-key" {
		t.Fatalf("AuthMode = %q, want api-key", spec.AuthMode)
	}
}

func TestProviderSpecCarriesExplicitMultiProviderConfig(t *testing.T) {
	spec := ProviderSpec(Config{
		Provider:                 "multi",
		DefaultProvider:          "openai",
		Model:                    "openai/gpt-5.5",
		AuthMode:                 "oauth",
		OpenAIOAuthPath:          "/oauth/auth.json",
		OpenAIOAuthAccountIDPath: "/oauth/account-id",
		ProviderAPIKeys:          map[string]string{"anthropic": "sk-ant-test"},
		ProviderBaseURLs:         map[string]string{"gemini": "https://gemini.example.test/v1"},
		ProviderAPIModes:         map[string]string{"gemini": "chat-completions"},
	})
	if spec.Provider != "multi" || spec.DefaultProvider != "openai" {
		t.Fatalf("provider config = %q/%q, want multi/openai", spec.Provider, spec.DefaultProvider)
	}
	if spec.AuthMode != "oauth" || spec.OpenAIOAuthPath != "/oauth/auth.json" || spec.OpenAIOAuthAccountIDPath != "/oauth/account-id" {
		t.Fatalf("oauth config = %+v", spec)
	}
	if spec.ProviderAPIKeys["anthropic"] != "sk-ant-test" {
		t.Fatalf("ProviderAPIKeys = %#v", spec.ProviderAPIKeys)
	}
	if spec.ProviderBaseURLs["gemini"] != "https://gemini.example.test/v1" {
		t.Fatalf("ProviderBaseURLs = %#v", spec.ProviderBaseURLs)
	}
}

func TestBuildToolBundleIncludesSDKAndSignalTools(t *testing.T) {
	cfg := Config{
		WorkDir:     t.TempDir(),
		ToolAccess:  agentsdk.ToolAccessLevelFull,
		EnableTools: true,
	}
	bundle, err := BuildToolBundle(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range bundle.Tools {
		names[tool.Name()] = true
	}
	for _, want := range []string{"Bash", "Write", "Edit", "LSP", "WebFetch", "AskUserQuestion", "present_plan", "finish", "set_phase", "list_files", "read_file", "glob", "grep"} {
		if !names[want] {
			t.Fatalf("missing tool %q; names=%v", want, names)
		}
	}
}

func TestBuildToolBundleCanDisableWebFetch(t *testing.T) {
	bundle, err := BuildToolBundle(context.Background(), Config{
		WorkDir:            t.TempDir(),
		ToolAccess:         agentsdk.ToolAccessLevelFull,
		EnableTools:        true,
		DisableSignalTools: true,
		DisableWebTools:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range bundle.Tools {
		names[tool.Name()] = true
	}
	if names["WebFetch"] {
		t.Fatalf("WebFetch present with DisableWebTools; names=%v", names)
	}
	if !names["Bash"] || !names["read_file"] {
		t.Fatalf("expected core tools with web disabled; names=%v", names)
	}
}

func TestBuildToolBundleWiresCommandSandboxConfig(t *testing.T) {
	bundle, err := BuildToolBundle(context.Background(), Config{
		WorkDir:              t.TempDir(),
		ToolAccess:           agentsdk.ToolAccessLevelFull,
		EnableTools:          true,
		DisableSignalTools:   true,
		CommandSandboxConfig: &sandbox.Config{Mode: "disabled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var bash *shell.WorkspaceWriteBashTool
	for _, tool := range bundle.Tools {
		if typed, ok := tool.(*shell.WorkspaceWriteBashTool); ok {
			bash = typed
			break
		}
	}
	if bash == nil {
		t.Fatalf("missing workspace-write Bash tool; tools=%v", toolNames(bundle.Tools))
	}
	if bash.Executor == nil {
		t.Fatal("Bash executor = nil, want runtime command sandbox config wired")
	}
}

func TestBuildToolBundleIncludesProjectStateTools(t *testing.T) {
	bundle, err := BuildToolBundle(context.Background(), Config{
		WorkDir:             t.TempDir(),
		EnableTools:         true,
		EnableProjectState:  true,
		ProjectID:           "test-project",
		ProjectStateDir:     filepath.Join(t.TempDir(), "state"),
		DisableDefaultTools: true,
		DisableSignalTools:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range bundle.Tools {
		names[tool.Name()] = true
	}
	for _, want := range []string{
		"task_create",
		"task_ready",
		"task_claim",
		"task_close",
		"memory_remember",
		"memory_recall",
		"memory_update",
		"memory_delete",
		"memory_stats",
		"prime_context",
	} {
		if !names[want] {
			t.Fatalf("missing project state tool %q; names=%v", want, names)
		}
	}
}

func TestBuildToolBundleCanUseOnlyHostTools(t *testing.T) {
	hostTool := staticTool{name: "host_tool"}
	bundle, err := BuildToolBundle(context.Background(), Config{
		WorkDir:             t.TempDir(),
		EnableTools:         true,
		DisableDefaultTools: true,
		DisableSignalTools:  true,
		ExtraTools:          []agentsdk.Tool{hostTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tools) != 1 || bundle.Tools[0].Name() != "host_tool" {
		t.Fatalf("tools = %v, want only host_tool", toolNames(bundle.Tools))
	}
}

func TestBuildToolBundleWiresOpenAIVisionAnalyzer(t *testing.T) {
	visionTool := &sdkvision.Tool{}
	bundle, err := BuildToolBundle(context.Background(), Config{
		Provider:            "openai",
		APIKey:              "sk-test",
		WorkDir:             t.TempDir(),
		EnableTools:         true,
		DisableDefaultTools: true,
		DisableSignalTools:  true,
		ExtraTools:          []agentsdk.Tool{visionTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Tools) != 1 || bundle.Tools[0] != visionTool {
		t.Fatalf("tools = %v, want supplied vision tool", toolNames(bundle.Tools))
	}
	if visionTool.AnalyzeWithDetailFn == nil {
		t.Fatal("AnalyzeWithDetailFn was not wired")
	}
}

func TestBuildToolBundleDoesNotOverrideCustomVisionAnalyzer(t *testing.T) {
	custom := sdkvision.AnalyzeWithDetailFn(func(context.Context, []byte, string, string, string) (string, error) {
		return "custom", nil
	})
	visionTool := &sdkvision.Tool{AnalyzeWithDetailFn: custom}
	_, err := BuildToolBundle(context.Background(), Config{
		Provider:            "openai",
		APIKey:              "sk-test",
		WorkDir:             t.TempDir(),
		EnableTools:         true,
		DisableDefaultTools: true,
		DisableSignalTools:  true,
		ExtraTools:          []agentsdk.Tool{visionTool},
	})
	if err != nil {
		t.Fatal(err)
	}
	if visionTool.AnalyzeWithDetailFn == nil {
		t.Fatal("custom analyzer was removed")
	}
	got, err := visionTool.AnalyzeWithDetailFn(context.Background(), nil, "", "", "high")
	if err != nil || got != "custom" {
		t.Fatalf("custom analyzer = %q, %v; want custom nil", got, err)
	}
}

func TestBuildToolBundleUsesProvidedMCPConfig(t *testing.T) {
	cfg := Config{
		WorkDir:   t.TempDir(),
		EnableMCP: true,
		MCPConfig: &sdkmcp.Config{MCPServers: map[string]sdkmcp.ServerConfig{
			"bad": {Type: "http"},
		}},
	}
	_, err := BuildToolBundle(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected unsupported MCP config error")
	}
	if !strings.Contains(err.Error(), `MCP server "bad" uses unsupported type "http"`) {
		t.Fatalf("error = %q, want provided MCP config validation", err.Error())
	}
}

func TestBuilderBuildsRunnableBundleShape(t *testing.T) {
	cfg := Config{
		Provider:               "openai",
		Model:                  "gpt-test",
		APIKey:                 "sk-test",
		WorkDir:                t.TempDir(),
		AgentName:              "test-agent",
		Instructions:           "test instructions",
		EnableTools:            true,
		EnableSubAgents:        true,
		EnableCompaction:       true,
		EnableGuardrails:       true,
		MaxTurns:               3,
		SubAgentMaxTurns:       2,
		ToolAccess:             agentsdk.ToolAccessLevelReadOnly,
		MaxConcurrentSubAgents: 1,
		ModeSnapshot: &sdkmode.TemplateSpec{Phases: []sdkmode.Phase{
			{ID: "plan"},
			{ID: "ship"},
		}},
	}
	bundle, err := NewBuilder(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Runner == nil || bundle.Agent == nil || bundle.Tracker == nil {
		t.Fatalf("bundle missing core fields: %+v", bundle)
	}
	if bundle.Config.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly {
		t.Fatalf("ToolAccessLevel = %q", bundle.Config.ToolAccessLevel)
	}
	if bundle.Config.MaxTurns != 3 || bundle.Config.SubAgentMaxTurns != 2 {
		t.Fatalf("run config turns = %d/%d", bundle.Config.MaxTurns, bundle.Config.SubAgentMaxTurns)
	}
	if len(bundle.Tools) == 0 {
		t.Fatal("expected tools")
	}
	if bundle.SpecialistAgents == nil {
		t.Fatal("expected specialist agent map")
	}
	names := map[string]bool{}
	for _, tool := range bundle.Agent.Tools {
		names[tool.Name()] = true
	}
	for _, want := range []string{"agent_agent", "spawn_subagent_task", "spawn_subagent_graph", "collect_subagent_result"} {
		if !names[want] {
			t.Fatalf("missing sub-agent tool %q; names=%v", want, toolNames(bundle.Agent.Tools))
		}
	}
	if names["wait_for_subagent_progress"] || names["wait_for_subagent_change"] {
		t.Fatalf("default runtime should not expose polling wait tools; names=%v", toolNames(bundle.Agent.Tools))
	}
	if bundle.SessionState == nil || bundle.SessionState.SubAgentScheduler() == nil {
		t.Fatal("expected session state with async sub-agent scheduler")
	}
	instructions := bundle.Agent.InstructionsFn(&agentsdk.RunContext{Config: bundle.Config}, bundle.Agent)
	if !strings.Contains(instructions, "This mode defines phases: plan -> ship") {
		t.Fatalf("instructions missing phase tracking directive:\n%s", instructions)
	}
	if !strings.Contains(instructions, "Turn budget: 3 LLM turns for this top-level run") {
		t.Fatalf("instructions missing top-level run budget:\n%s", instructions)
	}
	if strings.Contains(instructions, "<sub_agent_budget>") || strings.Contains(instructions, "5 LLM turns for this sub-agent") {
		t.Fatalf("top-level instructions leaked sub-agent budget:\n%s", instructions)
	}
}

func TestBuilderInjectsProjectStatePrimeContext(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	bundle, err := NewBuilder(Config{
		Provider:            "openai",
		Model:               "gpt-test",
		APIKey:              "sk-test",
		WorkDir:             t.TempDir(),
		AgentName:           "test-agent",
		EnableTools:         true,
		EnableProjectState:  true,
		ProjectID:           "test-project",
		ProjectStateDir:     stateDir,
		DisableDefaultTools: true,
		DisableSignalTools:  true,
		WorkingStateText:    "host working state",
		ProjectStateActor:   "test-agent",
		MaxTurns:            1,
		ToolAccess:          agentsdk.ToolAccessLevelFull,
	}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bundle.Config.WorkingStateContext, "host working state") || !strings.Contains(bundle.Config.WorkingStateContext, "Durable Project State") {
		t.Fatalf("WorkingStateContext = %q", bundle.Config.WorkingStateContext)
	}
	names := map[string]bool{}
	for _, tool := range bundle.Agent.Tools {
		names[tool.Name()] = true
	}
	if !names["task_create"] || !names["prime_context"] {
		t.Fatalf("missing project state tools: %v", toolNames(bundle.Agent.Tools))
	}
}

func TestBuilderReusesSessionStateSubAgentSchedulerAcrossBuilds(t *testing.T) {
	state := NewSessionState()
	cfg := Config{
		Provider:           "openai",
		Model:              "gpt-test",
		APIKey:             "sk-test",
		WorkDir:            t.TempDir(),
		EnableTools:        true,
		EnableSubAgents:    true,
		SubAgentMaxTurns:   2,
		ToolAccess:         agentsdk.ToolAccessLevelFull,
		SessionState:       state,
		DisableSignalTools: true,
	}

	first, err := NewBuilder(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	firstScheduler := state.SubAgentScheduler()
	if firstScheduler == nil {
		t.Fatal("first build did not create scheduler")
	}

	cfg.ToolAccess = agentsdk.ToolAccessLevelReadOnly
	second, err := NewBuilder(cfg).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.SubAgentScheduler() != firstScheduler {
		t.Fatal("expected scheduler to be reused across builds")
	}
	if first.SessionState != state || second.SessionState != state {
		t.Fatal("bundle did not carry the supplied session state")
	}
}

func TestBuildRunConfigHonorsHostOverrides(t *testing.T) {
	settings := agentsdk.ModelSettings{ReasoningEffort: "high", MaxTokens: 123}
	compaction := agentsdk.CompactionConfig{Enabled: true, TriggerTokens: 1000, TargetTokens: 500}
	handoffHistory := agentsdk.HandoffHistoryConfig{
		Enabled:             true,
		MaxTokens:           900,
		TargetTokens:        300,
		PreserveRecentItems: 4,
		SummaryBulletLimit:  2,
	}
	trace := agentsdk.NewTrace("host-trace")
	var recorded bool
	var reported bool
	var polled bool

	runCfg := BuildRunConfig(Config{
		Provider:                  "multi",
		Model:                     "gpt-test",
		WorkDir:                   "/repo",
		ActiveMode:                "ultrawork",
		ActivePhase:               "shipping",
		ModelSettings:             &settings,
		MaxTurns:                  7,
		SubAgentMaxTurns:          3,
		MaxConcurrentSubAgents:    2,
		ToolAccess:                agentsdk.ToolAccessLevelReadOnly,
		WorkingStateText:          "working state",
		ModeDirectiveText:         "mode instructions",
		Trace:                     trace,
		ParentSpanID:              "parent-span",
		CompactionConfig:          &compaction,
		HandoffHistory:            &handoffHistory,
		ImmediateInputPoller:      func(context.Context) ([]agentsdk.RunItem, error) { polled = true; return nil, nil },
		CompactionRecorder:        func(_, _ int, _ string) { recorded = true },
		CompactionFailureReporter: func(_, _ string, _, _ int) { reported = true },
		CompactionCarryForward:    func(context.Context) string { return "carry forward" },
	}, nil)

	if runCfg.ModelSettings.ReasoningEffort != "high" || runCfg.ModelSettings.MaxTokens != 123 {
		t.Fatalf("ModelSettings = %+v", runCfg.ModelSettings)
	}
	if runCfg.MaxTurns != 7 || runCfg.SubAgentMaxTurns != 3 || runCfg.MaxConcurrentSubAgents != 2 {
		t.Fatalf("turn config = %+v", runCfg)
	}
	if runCfg.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly || runCfg.WorkingStateContext != "working state" || runCfg.AdditionalInstructions != "mode instructions" {
		t.Fatalf("context config = %+v", runCfg)
	}
	if runCfg.Trace != trace || runCfg.ParentSpanID != "parent-span" {
		t.Fatalf("trace config = trace:%v parent:%q", runCfg.Trace, runCfg.ParentSpanID)
	}
	if !runCfg.CompactionConfig.Enabled || runCfg.CompactionConfig.TriggerTokens != 1000 {
		t.Fatalf("CompactionConfig = %+v", runCfg.CompactionConfig)
	}
	if !runCfg.HandoffHistory.Enabled || runCfg.HandoffHistory.MaxTokens != 900 {
		t.Fatalf("HandoffHistory = %+v", runCfg.HandoffHistory)
	}
	if got := runCfg.CompactionCarryForward(context.Background()); got != "carry forward" {
		t.Fatalf("CompactionCarryForward() = %q", got)
	}
	runCfg.CompactionRecorder(1, 2, "summary")
	runCfg.CompactionFailureReporter("scope", "reason", 1, 2)
	if _, err := runCfg.ImmediateInputPoller(context.Background()); err != nil {
		t.Fatalf("ImmediateInputPoller() error = %v", err)
	}
	if !recorded || !reported || !polled {
		t.Fatalf("callbacks recorded=%v reported=%v polled=%v", recorded, reported, polled)
	}
}

type staticTool struct {
	name string
}

func (t staticTool) Name() string                        { return t.name }
func (t staticTool) Description() string                 { return "static test tool" }
func (t staticTool) InputSchema() json.RawMessage        { return json.RawMessage(`{"type":"object"}`) }
func (t staticTool) IsReadOnly() bool                    { return true }
func (t staticTool) IsEnabled(*agentsdk.RunContext) bool { return true }
func (t staticTool) NeedsApproval() bool                 { return false }
func (t staticTool) TimeoutSeconds() int                 { return 0 }
func (t staticTool) Execute(context.Context, json.RawMessage, string) (agentsdk.ToolResult, error) {
	return agentsdk.ToolResult{Content: "ok"}, nil
}

func TestModeOverridesFromSnapshot(t *testing.T) {
	overrides := ModeOverridesFromSnapshot(&sdkmode.TemplateSpec{
		Instructions: "snapshot instructions",
		ModelRouting: &sdkmode.ModelRouting{
			DefaultModel:   "gpt-special",
			ReasoningLevel: "high",
			TextVerbosity:  "low",
		},
		Constraints: &sdkmode.Constraints{
			MaxTurns:               11,
			SubAgentMaxTurns:       5,
			MaxConcurrentSubAgents: 2,
		},
	}, "live instructions")

	if overrides.Model != "gpt-special" {
		t.Fatalf("Model = %q", overrides.Model)
	}
	if overrides.Reasoning != "high" || overrides.ModelSettings.ReasoningEffort != "high" || overrides.ModelSettings.TextVerbosity != "low" {
		t.Fatalf("model settings = %+v reasoning=%q", overrides.ModelSettings, overrides.Reasoning)
	}
	if overrides.MaxTurns != 11 || overrides.SubAgentMaxTurns != 5 || overrides.MaxConcurrentSubAgents != 2 {
		t.Fatalf("constraints = %+v", overrides)
	}
	if overrides.ModeInstructions != "live instructions" {
		t.Fatalf("ModeInstructions = %q", overrides.ModeInstructions)
	}

	overrides = ModeOverridesFromSnapshot(&sdkmode.TemplateSpec{Instructions: "snapshot instructions"}, "")
	if overrides.ModeInstructions != "snapshot instructions" {
		t.Fatalf("fallback ModeInstructions = %q", overrides.ModeInstructions)
	}
}

func TestBuildCatalogHandoffsTargetsSpecialists(t *testing.T) {
	cfg := Config{
		Model: "openai/gpt-5.5",
		RoleCatalog: agentsdk.RoleCatalog{
			{Name: "reviewer", Description: "Reviews code changes."},
			{Name: "release planner", Description: "Plans releases."},
			{Name: "missing"},
		},
	}
	specialists := map[string]*agentsdk.Agent{
		"reviewer":        {Name: "reviewer"},
		"release planner": {Name: "release planner"},
	}

	handoffs := buildCatalogHandoffs(cfg, specialists)
	if len(handoffs) != 2 {
		t.Fatalf("len(handoffs) = %d, want 2", len(handoffs))
	}
	if handoffs[0].ToolName != "transfer_to_reviewer" {
		t.Errorf("handoffs[0].ToolName = %q, want transfer_to_reviewer", handoffs[0].ToolName)
	}
	if handoffs[0].Agent != specialists["reviewer"] {
		t.Errorf("handoffs[0] does not target the reviewer specialist")
	}
	if handoffs[1].ToolName != "transfer_to_release_planner" {
		t.Errorf("handoffs[1].ToolName = %q, want transfer_to_release_planner", handoffs[1].ToolName)
	}
	if handoffs[0].Description != "Reviews code changes." {
		t.Errorf("handoffs[0].Description = %q", handoffs[0].Description)
	}
}

func TestBuildCatalogHandoffsFallsBackToGenericSpecialist(t *testing.T) {
	cfg := Config{Model: "openai/gpt-5.5"}
	handoffs := buildCatalogHandoffs(cfg, nil)
	if len(handoffs) != 1 {
		t.Fatalf("len(handoffs) = %d, want 1", len(handoffs))
	}
	if handoffs[0].ToolName != "transfer_to_specialist" {
		t.Errorf("ToolName = %q, want transfer_to_specialist", handoffs[0].ToolName)
	}
	if handoffs[0].Agent == nil || handoffs[0].Agent.Name != "specialist" {
		t.Errorf("fallback handoff should target the generic specialist agent")
	}
}

func TestSanitizeHandoffName(t *testing.T) {
	cases := map[string]string{
		"reviewer":        "reviewer",
		"Release Planner": "release_planner",
		"deep-dive.v2":    "deep_dive_v2",
		"  spaced  ":      "spaced",
		"!!!":             "specialist",
	}
	for in, want := range cases {
		if got := sanitizeHandoffName(in); got != want {
			t.Errorf("sanitizeHandoffName(%q) = %q, want %q", in, got, want)
		}
	}
}
