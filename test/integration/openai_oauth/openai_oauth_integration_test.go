package openai_oauth_integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkevents "github.com/gratefulagents/sdk/pkg/agentsdk/events"
	sdkguardrails "github.com/gratefulagents/sdk/pkg/agentsdk/guardrails"
	"github.com/gratefulagents/sdk/pkg/agentsdk/host/fileconfig"
	sdkmcp "github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	sdkmemory "github.com/gratefulagents/sdk/pkg/agentsdk/memory"
	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
	sdkpolicy "github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	sdkproviders "github.com/gratefulagents/sdk/pkg/agentsdk/providers"
	sdkanthropic "github.com/gratefulagents/sdk/pkg/agentsdk/providers/anthropic"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
	sdkruntime "github.com/gratefulagents/sdk/pkg/agentsdk/runtime"
	sdksandbox "github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	sdktools "github.com/gratefulagents/sdk/pkg/agentsdk/tools"
	sdksignal "github.com/gratefulagents/sdk/pkg/agentsdk/tools/signal"
	sdktracestore "github.com/gratefulagents/sdk/pkg/agentsdk/tracestore"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const liveOpenAIModel = "gpt-5.5"

type integrationDecision struct {
	Answer string `json:"answer"`
	Source string `json:"source"`
}

func TestLiveOpenAIOAuthRuntimeToolsStreamingStructuredOutputGuardrailsHooksAndUsage(t *testing.T) {
	ctx := context.Background()
	runner, model := liveOpenAIRunner(t)

	schema := agentsdk.NewOutputSchema("integration_decision", json.RawMessage(`{
		"type":"object",
		"properties":{
			"answer":{"type":"string"},
			"source":{"type":"string"}
		},
		"required":["answer","source"],
		"additionalProperties":false
	}`))
	schema.Strict = true
	schema.ParseFn = func(raw string) (any, error) {
		var out integrationDecision
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, err
		}
		return out, nil
	}

	var toolCalled bool
	lookup := &agentsdk.FunctionTool{
		ToolName:        "lookup_fact",
		ToolDescription: "Returns the canonical SDK integration runtime status.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"topic":{"type":"string"}},"required":["topic"],"additionalProperties":false}`),
		ReadOnly:        true,
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			var in struct {
				Topic string `json:"topic"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "", err
			}
			toolCalled = true
			return "runtime ready via " + in.Topic, nil
		},
	}

	hooks := &recordingRunHooks{}
	streamed := runner.RunStreamed(ctx, &agentsdk.Agent{
		Name:       "oauth-integration-live",
		Model:      model,
		OutputType: schema,
		Instructions: strings.Join([]string{
			"You are running a live SDK integration test.",
			"First call lookup_fact with topic set to runtime.",
			"After the tool result, return only JSON matching the schema.",
			`Use answer "runtime ready" and source "lookup_fact".`,
		}, " "),
		Tools: []agentsdk.Tool{lookup},
		InputGuardrails: []agentsdk.InputGuardrail{{
			Name: "input-present",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, input []agentsdk.RunItem) (*agentsdk.GuardrailResult, error) {
				return &agentsdk.GuardrailResult{Output: len(input), TripwireTriggered: len(input) == 0}, nil
			},
		}},
		OutputGuardrails: []agentsdk.OutputGuardrail{{
			Name: "typed-output",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, output any) (*agentsdk.GuardrailResult, error) {
				_, ok := output.(integrationDecision)
				return &agentsdk.GuardrailResult{Output: "typed", TripwireTriggered: !ok}, nil
			},
		}},
	}, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Use the lookup tool and report the runtime status."},
	}}, agentsdk.RunConfig{
		MaxTurns: 3,
		Hooks:    hooks,
		ModelSettings: agentsdk.ModelSettings{
			MaxTokens:       256,
			ReasoningEffort: string(agentsdk.ReasoningLow),
			TextVerbosity:   string(agentsdk.TextVerbosityLow),
		},
		ToolInputGuardrails: []agentsdk.ToolInputGuardrail{{
			Name: "tool-topic-required",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, tool agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
				return &agentsdk.GuardrailResult{
					Output:            tool.Name(),
					TripwireTriggered: !strings.Contains(string(input), "runtime"),
				}, nil
			},
		}},
		ToolOutputGuardrails: []agentsdk.ToolOutputGuardrail{{
			Name: "tool-output-nonempty",
			Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
				return &agentsdk.GuardrailResult{Output: len(result.Content), TripwireTriggered: result.Content == ""}, nil
			},
		}},
		WorkDir: t.TempDir(),
	})

	var runItems int
	for event := range streamed.Events {
		if event.Type == agentsdk.StreamEventRunItem {
			runItems++
		}
	}
	result := streamed.FinalResult()
	out, ok := result.FinalOutput.(integrationDecision)
	if !ok {
		t.Fatalf("FinalOutput = %T (%#v), want integrationDecision", result.FinalOutput, result.FinalOutput)
	}
	if strings.ToLower(out.Answer) != "runtime ready" || out.Source != "lookup_fact" {
		t.Fatalf("decision = %+v", out)
	}
	if !toolCalled || runItems == 0 {
		t.Fatalf("toolCalled=%v runItems=%d", toolCalled, runItems)
	}
	if !hasRunItemType(result.NewItems, agentsdk.RunItemToolCall) || !hasRunItemType(result.NewItems, agentsdk.RunItemToolOutput) || !hasRunItemType(result.NewItems, agentsdk.RunItemMessage) {
		t.Fatalf("result items do not prove tool-call lifecycle: %#v", result.NewItems)
	}
	if len(result.InputGuardrailResults) != 1 || len(result.OutputGuardrailResults) != 1 ||
		len(result.ToolInputGuardrailResults) != 1 || len(result.ToolOutputGuardrailResults) != 1 {
		t.Fatalf("guardrail results missing: %+v", result)
	}
	if hooks.agentStarts == 0 || hooks.agentEnds == 0 || hooks.llmStarts == 0 || hooks.llmEnds == 0 || hooks.toolStarts == 0 || hooks.toolEnds == 0 {
		t.Fatalf("hooks did not observe runtime lifecycle: %+v", hooks)
	}
	if result.Usage.Requests == 0 || result.Usage.InputTokens == 0 || len(result.RawResponses) == 0 {
		t.Fatalf("usage/raw responses missing: usage=%+v raw=%d", result.Usage, len(result.RawResponses))
	}
}

func TestLiveOpenAIOAuthRuntimeBuilderChatLoopHandoffsSyncAndAsyncSubagents(t *testing.T) {
	ctx := context.Background()
	live := liveOpenAIConfig(t)
	settings := agentsdk.ModelSettings{MaxTokens: 128, ReasoningEffort: string(agentsdk.ReasoningLow), TextVerbosity: string(agentsdk.TextVerbosityLow)}

	bundle, err := sdkruntime.NewBuilder(sdkruntime.Config{
		Provider:         "openai",
		Model:            liveOpenAIModel,
		BaseURL:          live.baseURL,
		AuthMode:         string(sdkopenai.AuthModeOAuth),
		OpenAIOAuthPath:  live.authPath,
		AgentName:        "builder-live",
		Instructions:     "Reply with exactly: builder live ok",
		ModelSettings:    &settings,
		MaxTurns:         1,
		EnableTools:      false,
		EnableMCP:        false,
		EnableHandoffs:   false,
		EnableSubAgents:  false,
		EnableGuardrails: false,
		WorkDir:          t.TempDir(),
	}).Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	builderResult, err := bundle.Runner.Run(ctx, bundle.Agent, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Say the exact builder test phrase."},
	}}, bundle.Config)
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(builderResult.FinalText()) != "builder live ok" {
		t.Fatalf("builder FinalText() = %q", builderResult.FinalText())
	}

	runner, model := liveOpenAIRunner(t)
	session := &integrationSession{messages: []agentsdk.UserMessage{{ID: 1, Content: "Say the exact loop test phrase."}}}
	loopResult, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
		Runner:       runner,
		SessionStore: session,
		Agent: &agentsdk.Agent{
			Name:         "loop-live",
			Model:        model,
			Instructions: "Reply with exactly: loop live ok",
		},
		RunConfig: agentsdk.RunConfig{MaxTurns: 1, ModelSettings: settings},
	}).Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(loopResult.FinalText()) != "loop live ok" || len(session.items) == 0 {
		t.Fatalf("loop FinalText()=%q persisted=%d", loopResult.FinalText(), len(session.items))
	}

	reviewer := &agentsdk.Agent{
		Name:         "reviewer-live",
		Model:        model,
		Instructions: "Reply with exactly: reviewed live ok",
	}
	triage := &agentsdk.Agent{
		Name:         "triage-live",
		Model:        model,
		Instructions: "Call transfer_to_reviewer now. Do not answer directly.",
		Handoffs:     []*agentsdk.Handoff{agentsdk.NewHandoff(reviewer, agentsdk.WithToolName("transfer_to_reviewer"))},
	}
	handoffResult, err := runner.Run(ctx, triage, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Route this to the reviewer."},
	}}, agentsdk.RunConfig{MaxTurns: 2, ModelSettings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(handoffResult.FinalText()) != "reviewed live ok" || handoffResult.LastAgent.Name != "reviewer-live" {
		t.Fatalf("handoff text=%q last=%s", handoffResult.FinalText(), handoffResult.LastAgent.Name)
	}

	researcher := &agentsdk.Agent{
		Name:         "researcher_live",
		Model:        model,
		Instructions: "Reply with exactly: nested live ok",
	}
	parent := &agentsdk.Agent{
		Name:         "parent-live",
		Model:        model,
		Instructions: "Call agent_researcher_live now with message set to nested live ok. After the tool result, reply with exactly: parent got nested live ok",
		Tools:        []agentsdk.Tool{researcher.AsTool(runner)},
	}
	subagentResult, err := runner.Run(ctx, parent, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Delegate this to the researcher."},
	}}, agentsdk.RunConfig{MaxTurns: 3, SubAgentMaxTurns: 1, ModelSettings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(subagentResult.FinalText()) != "parent got nested live ok" {
		t.Fatalf("subagent text=%q", subagentResult.FinalText())
	}

	asyncWorker := &agentsdk.Agent{
		Name:         "async_worker",
		Model:        model,
		Instructions: "Reply with exactly: async nested live ok",
	}
	scheduler := agentsdk.NewSubAgentScheduler(agentsdk.SubAgentSchedulerConfig{
		MaxConcurrent:   1,
		Runner:          runner,
		Agents:          map[string]*agentsdk.Agent{"async_worker": asyncWorker},
		WorkDir:         t.TempDir(),
		ToolAccessLevel: agentsdk.ToolAccessLevelReadOnly,
		MaxTurns:        1,
	})
	asyncTools := agentsdk.BuildSubAgentTaskTools(scheduler, "async_worker")
	spawnResult, err := toolByName(t, asyncTools, "subagent").Execute(ctx, json.RawMessage(`{"agent_name":"async_worker","message":"Say the exact async test phrase.","tool_access":"read-only","mode":"background"}`), "")
	if err != nil || spawnResult.IsError {
		t.Fatalf("spawn async sub-agent result=%+v err=%v", spawnResult, err)
	}
	var spawned struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(spawnResult.Content), &spawned); err != nil || spawned.TaskID == "" {
		t.Fatalf("spawn payload=%q err=%v", spawnResult.Content, err)
	}
	if _, err := scheduler.WaitForTask(ctx, spawned.TaskID, 60000); err != nil {
		t.Fatalf("wait async sub-agent task: %v", err)
	}
	activityResult, err := toolByName(t, asyncTools, "subagent_status").Execute(ctx, json.RawMessage(fmt.Sprintf(`{"detail":"activity","task_ids":[%q]}`, spawned.TaskID)), "")
	if err != nil || activityResult.IsError || !strings.Contains(activityResult.Content, `"agent": "async_worker"`) {
		t.Fatalf("activity result=%+v err=%v", activityResult, err)
	}
	// Results auto-deliver to managed parents; here we read the terminal task's
	// result directly from the scheduler (the same path auto-delivery uses)
	// now that the blocking collect_subagent_result tool has been removed.
	collected, err := scheduler.CollectResult(spawned.TaskID)
	if err != nil || normalizeText(collected.Result) != "async nested live ok" {
		t.Fatalf("collect async sub-agent result=%+v err=%v", collected, err)
	}
}

func TestLiveOpenAIOAuthProvidersPoliciesModesRoutingToolRegistryAndSecurity(t *testing.T) {
	ctx := context.Background()
	live := liveOpenAIConfig(t)
	workDir := t.TempDir()

	provider, err := sdkproviders.NewProviderFromConfig(sdkproviders.ProviderSpec{
		Provider:        "openai",
		Model:           liveOpenAIModel,
		BaseURL:         live.baseURL,
		AuthMode:        string(sdkopenai.AuthModeOAuth),
		OpenAIOAuthPath: live.authPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	model, err := provider.GetModel(liveOpenAIModel)
	if err != nil {
		t.Fatal(err)
	}
	if model.Provider() != "openai" {
		t.Fatalf("provider = %q", model.Provider())
	}
	liveResult, err := agentsdk.NewRunnerWithProvider(provider).Run(ctx, &agentsdk.Agent{
		Name:         "provider-live",
		Model:        liveOpenAIModel,
		Instructions: "Reply with exactly: provider live ok",
	}, []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Say the provider phrase."}}}, agentsdk.RunConfig{
		MaxTurns:      1,
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 96, ReasoningEffort: string(agentsdk.ReasoningLow)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(liveResult.FinalText()) != "provider live ok" {
		t.Fatalf("provider FinalText() = %q", liveResult.FinalText())
	}

	modelStream, err := model.StreamResponse(ctx, agentsdk.ModelRequest{
		Model:        liveOpenAIModel,
		Instructions: "Reply with exactly: low level stream ok",
		Input: []agentsdk.RunItem{{
			Type:    agentsdk.RunItemMessage,
			Message: &agentsdk.MessageOutput{Text: "Say the low-level stream phrase."},
		}},
		Settings: agentsdk.ModelSettings{MaxTokens: 96, ReasoningEffort: string(agentsdk.ReasoningLow)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var streamDelta string
	for event := range modelStream.Events {
		switch event.Type {
		case agentsdk.ModelStreamDelta:
			streamDelta += event.Delta
		}
	}
	streamFinal := modelStream.Final()
	if streamFinal == nil || streamFinal.Usage.Requests == 0 || streamDelta == "" || !strings.Contains(normalizeText(runItemsText(streamFinal.Items)+streamDelta), "low level stream ok") {
		t.Fatalf("model stream final=%+v delta=%q", streamFinal, streamDelta)
	}
	compactor, ok := model.(agentsdk.ContextCompactor)
	if !ok || !compactor.SupportsContextCompaction() {
		t.Fatalf("%s OAuth model should expose provider-native context compaction", liveOpenAIModel)
	}
	nativeCompacted, err := compactor.CompactContext(ctx, agentsdk.ModelRequest{
		Model:        liveOpenAIModel,
		Instructions: "Compact the conversation into a concise state while preserving named integration details.",
		Input: []agentsdk.RunItem{
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Remember alpha integration detail."}},
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Remember beta integration detail."}},
			{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Compact this conversation while preserving the integration details."}},
		},
		Settings: agentsdk.ModelSettings{MaxTokens: 128, ReasoningEffort: string(agentsdk.ReasoningLow)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if nativeCompacted == nil || nativeCompacted.Usage.Requests == 0 || nativeCompacted.Raw == nil || (nativeCompacted.Summary == "" && len(nativeCompacted.Items) == 0) {
		t.Fatalf("native compaction = %+v", nativeCompacted)
	}

	var protectedCalled bool
	protectedTool := &agentsdk.FunctionTool{
		ToolName:        "mutate_protected",
		ToolDescription: "A mutating tool used to verify approval gates.",
		Schema:          json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`),
		ReadOnly:        false,
		Fn: func(context.Context, json.RawMessage) (string, error) {
			protectedCalled = true
			return "mutated", nil
		},
	}
	approvalResult, err := agentsdk.NewRunnerWithProvider(provider).Run(ctx, &agentsdk.Agent{
		Name:         "approval-live",
		Model:        liveOpenAIModel,
		Instructions: "Call mutate_protected now with value set to approval-required. Do not answer directly.",
		Tools:        []agentsdk.Tool{protectedTool},
	}, []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Verify the approval gate."}}}, agentsdk.RunConfig{
		MaxTurns:      1,
		ToolPolicy:    &agentsdk.ToolPolicy{ApprovalRequired: true, DefaultTimeout: 11},
		ModelSettings: agentsdk.ModelSettings{MaxTokens: 96, ReasoningEffort: string(agentsdk.ReasoningLow)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if approvalResult.Interruption == nil || approvalResult.Interruption.ToolName != "mutate_protected" || protectedCalled {
		t.Fatalf("approval interruption=%+v protectedCalled=%v", approvalResult.Interruption, protectedCalled)
	}

	modeSpec := &sdkmode.TemplateSpec{
		Name:        "ship-mode",
		DisplayName: "Ship Mode",
		Phases: []sdkmode.Phase{
			{ID: "plan", ReadOnly: true},
			{ID: "build", RequiresApproval: true},
		},
		ModelRouting: &sdkmode.ModelRouting{
			DefaultModel:   "openai/" + liveOpenAIModel,
			ReasoningLevel: string(agentsdk.ReasoningHigh),
			TextVerbosity:  string(agentsdk.TextVerbosityLow),
			RoleOverrides: map[string]sdkmode.RoleModelRouting{
				"writer": {Model: "openai/" + liveOpenAIModel, ReasoningLevel: string(agentsdk.ReasoningLow)},
			},
		},
		Constraints: &sdkmode.Constraints{MaxTurns: 4, SubAgentMaxTurns: 2, MaxConcurrentSubAgents: 1},
		ResetTo:     &sdkmode.ResetTo{Name: "chat", Prompt: "Continue in chat.", RequiresApproval: true},
	}
	if access, overridden := agentsdk.ResolvePhaseToolAccess(agentsdk.ToolAccessLevelFull, modeSpec, "plan"); !overridden || access != agentsdk.ToolAccessLevelReadOnly {
		t.Fatalf("phase access = %q overridden=%v", access, overridden)
	}
	if decision := agentsdk.EvaluatePhaseTurnLimit("build", 2, 2); !decision.Exceeded || len(decision.Actions) == 0 {
		t.Fatalf("phase turn decision = %+v", decision)
	}
	if completion := agentsdk.EvaluateModeCompletion("ship-mode", true, modeSpec.ResetTo); !completion.Completed || !completion.RequiresApproval || completion.TargetMode != "chat" {
		t.Fatalf("mode completion = %+v", completion)
	}
	if routing := agentsdk.ResolveRoleModeRouting("", "openai", "writer", agentsdk.ModeRoutingFromTemplateSpec(modeSpec)); routing.Model == "" || routing.ModelSettings.ReasoningEffort != "low" {
		t.Fatalf("role routing = %+v", routing)
	}
	denied := sdkmode.Evaluate(modeSpec, &sdkmode.TemplateSpec{Name: "admin-only", Category: "orchestrated"}, sdkmode.EvaluateOpts{ActorRole: sdkmode.RoleUser})
	if denied.Result != sdkmode.ResultDenied || denied.DenyCode != sdkmode.DenyRBACDenied {
		t.Fatalf("mode RBAC denial = %+v", denied)
	}
	if gate := sdkmode.EvaluatePhaseEntryGates([]sdkmode.PhaseGate{{Require: "plan_exists"}}, sdkmode.EvidenceContext{WorkDir: workDir}); gate == nil || gate.Passed {
		t.Fatalf("missing plan gate = %+v", gate)
	}
	if gate := sdkmode.EvaluatePhaseEntryGatesWithGitRunner([]sdkmode.PhaseGate{{Require: "git_clean"}}, sdkmode.EvidenceContext{WorkDir: workDir}, func(string, ...string) ([]byte, error) {
		return []byte(" M file.go\n"), nil
	}); gate == nil || gate.Passed || !strings.Contains(gate.Message, "uncommitted") {
		t.Fatalf("dirty git gate = %+v", gate)
	}
	specialists := agentsdk.BuildSpecialistsFromCatalog(agentsdk.RoleCatalog{{
		Name:         "writer",
		Description:  "Writes final copy.",
		Instructions: "Write clearly.",
		ToolAccess:   "read-only",
	}})
	if len(specialists) != 1 || specialists[0].Name != "writer" {
		t.Fatalf("specialists = %+v", specialists)
	}

	registry := sdktools.NewRegistry(
		workDir,
		sdktools.WithPermissionMode(sdkpolicy.PermissionModeWorkspaceWrite),
		sdktools.WithSignalTools(),
		sdktools.WithBrowserTools(),
		sdktools.WithVisionTools(func(_ context.Context, imageData []byte, mimeType, prompt string) (string, error) {
			return fmt.Sprintf("vision ok %s %d %s", mimeType, len(imageData), prompt), nil
		}),
		sdktools.WithMemoryStore(sdkmemory.NewInMemoryStore(), "integration", "run-1", "https://example.test/repo"),
	)
	for _, name := range []string{"AnalyzeImage", "AskUserQuestion", "Bash", "Browser", "Edit", "LSP", "Memory", "WebFetch", "Write", "glob", "grep", "list_files", "present_plan", "read_file"} {
		if registry.Get(name) == nil {
			t.Fatalf("registry missing %q; names=%v", name, registry.Names())
		}
	}
	writeResult, err := registry.Get("Write").Execute(ctx, json.RawMessage(`{"file_path":"notes.txt","content":"hello"}`), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if writeResult.IsError {
		t.Fatalf("Write returned error: %s", writeResult.Content)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "notes.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("written file = %q err=%v", string(data), err)
	}
	editResult, err := registry.Get("Edit").Execute(ctx, json.RawMessage(`{"file_path":"notes.txt","old_string":"hello","new_string":"hello edited"}`), workDir)
	if err != nil || editResult.IsError {
		t.Fatalf("Edit result=%+v err=%v", editResult, err)
	}
	readResult, err := registry.Get("read_file").Execute(ctx, json.RawMessage(`{"path":"notes.txt"}`), workDir)
	if err != nil || readResult.Content != "hello edited" {
		t.Fatalf("read_file result=%+v err=%v", readResult, err)
	}
	grepResult, err := registry.Get("grep").Execute(ctx, json.RawMessage(`{"pattern":"edited","glob":"*.txt"}`), workDir)
	if err != nil || !strings.Contains(grepResult.Content, "notes.txt:1") {
		t.Fatalf("grep result=%+v err=%v", grepResult, err)
	}
	globResult, err := registry.Get("glob").Execute(ctx, json.RawMessage(`{"pattern":"*.txt"}`), workDir)
	if err != nil || !strings.Contains(globResult.Content, "notes.txt") {
		t.Fatalf("glob result=%+v err=%v", globResult, err)
	}
	listResult, err := registry.Get("list_files").Execute(ctx, json.RawMessage(`{}`), workDir)
	if err != nil || !strings.Contains(listResult.Content, "notes.txt") {
		t.Fatalf("list_files result=%+v err=%v", listResult, err)
	}
	bashResult, err := registry.Get("Bash").Execute(ctx, json.RawMessage(`{"command":"printf shell-ok","timeout":5000}`), workDir)
	if err != nil || strings.TrimSpace(bashResult.Content) != "shell-ok" {
		t.Fatalf("Bash result=%+v err=%v", bashResult, err)
	}
	memoryStoreResult, err := registry.Get("Memory").Execute(ctx, json.RawMessage(`{"action":"store","content":"OAuth integration memory","tags":["integration"]}`), workDir)
	if err != nil || memoryStoreResult.IsError {
		t.Fatalf("Memory store result=%+v err=%v", memoryStoreResult, err)
	}
	var storedMemory struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(memoryStoreResult.Content), &storedMemory); err != nil || storedMemory.ID == "" {
		t.Fatalf("stored memory payload=%q err=%v", memoryStoreResult.Content, err)
	}
	memorySearchResult, err := registry.Get("Memory").Execute(ctx, json.RawMessage(`{"action":"search","content":"OAuth","tags":["integration"],"limit":2}`), workDir)
	if err != nil || memorySearchResult.IsError || !strings.Contains(memorySearchResult.Content, storedMemory.ID) {
		t.Fatalf("Memory search result=%+v err=%v", memorySearchResult, err)
	}
	memoryDeleteResult, err := registry.Get("Memory").Execute(ctx, json.RawMessage(fmt.Sprintf(`{"action":"delete","id":%q}`, storedMemory.ID)), workDir)
	if err != nil || memoryDeleteResult.IsError {
		t.Fatalf("Memory delete result=%+v err=%v", memoryDeleteResult, err)
	}
	questionResult, err := registry.Get("AskUserQuestion").Execute(ctx, json.RawMessage(`{"question":"Pick one","choices":["a","b"],"allow_freeform":false}`), workDir)
	if err != nil || !strings.Contains(questionResult.Content, `"allow_freeform":false`) {
		t.Fatalf("AskUserQuestion result=%+v err=%v", questionResult, err)
	}
	planResult, err := registry.Get("present_plan").Execute(ctx, json.RawMessage(`{"summary":"Ship safely","actions":[{"id":"approve","label":"Approve","style":"primary"}],"recommended":"approve"}`), workDir)
	if err != nil || planResult.IsError || !strings.Contains(planResult.Content, "Ship safely") {
		t.Fatalf("present_plan result=%+v err=%v", planResult, err)
	}
	must(t, os.WriteFile(filepath.Join(workDir, "pixel.png"), tinyPNG(), 0o644))
	visionResult, err := registry.Get("AnalyzeImage").Execute(ctx, json.RawMessage(`{"image_path":"pixel.png","prompt":"smoke","detail_level":"low"}`), workDir)
	if err != nil || visionResult.IsError || !strings.Contains(visionResult.Content, "vision ok image/png") {
		t.Fatalf("AnalyzeImage result=%+v err=%v", visionResult, err)
	}
	webResult, err := registry.Get("WebFetch").Execute(ctx, json.RawMessage(`{"url":"http://127.0.0.1:1/"}`), workDir)
	if err != nil || !webResult.IsError || !strings.Contains(strings.ToLower(webResult.Content), "private or local") {
		t.Fatalf("WebFetch SSRF result=%+v err=%v", webResult, err)
	}
	browserResult, err := registry.Get("Browser").Execute(ctx, json.RawMessage(`{"action":"navigate","url":"http://localhost:1/"}`), workDir)
	if err != nil || !browserResult.IsError || !strings.Contains(strings.ToLower(browserResult.Content), "private or local") {
		t.Fatalf("Browser SSRF result=%+v err=%v", browserResult, err)
	}
	escapeResult, err := registry.Get("Write").Execute(ctx, json.RawMessage(`{"file_path":"../escape.txt","content":"escaped"}`), workDir)
	if err != nil || !escapeResult.IsError {
		t.Fatalf("Write path escape result=%+v err=%v", escapeResult, err)
	}
	blockedShellResult, err := registry.Get("Bash").Execute(ctx, json.RawMessage(`{"command":"git remote add origin https://example.invalid/repo.git"}`), workDir)
	if err != nil || !blockedShellResult.IsError || !strings.Contains(blockedShellResult.Content, "blocked") {
		t.Fatalf("Bash security result=%+v err=%v", blockedShellResult, err)
	}
	readOnlyRegistry := sdktools.NewRegistry(workDir, sdktools.WithReadOnlyTools())
	if readOnlyRegistry.Get("Write") != nil || readOnlyRegistry.Get("Edit") != nil {
		t.Fatalf("read-only registry exposed write tools: %v", readOnlyRegistry.Names())
	}
	if readOnlyRegistry.Get("Bash") == nil || !readOnlyRegistry.Get("Bash").IsReadOnly() {
		t.Fatalf("read-only Bash missing or mutable: %v", readOnlyRegistry.Names())
	}
	guardedInput, _, errs := sdkguardrails.ToolGuardrailsFromRules([]agentsdk.GuardrailRule{{
		Name:        "block-secret-input",
		Type:        "tool-input",
		ToolPattern: "Bash",
		Regex:       `password\s*=`,
		Action:      "block",
		Message:     "secret blocked",
	}})
	if len(errs) != 0 || len(guardedInput) != 1 {
		t.Fatalf("compiled guardrails input=%d errs=%v", len(guardedInput), errs)
	}
	gr, err := guardedInput[0].Fn(&agentsdk.RunContext{}, &agentsdk.Agent{Name: "security"}, registry.Get("Bash"), json.RawMessage(`{"command":"echo password = secret"}`))
	if err != nil || gr == nil || !gr.TripwireTriggered {
		t.Fatalf("custom input guardrail=%+v err=%v", gr, err)
	}
	for _, builtin := range sdkguardrails.BuiltinToolInputGuardrails() {
		gr, err := builtin.Fn(&agentsdk.RunContext{}, &agentsdk.Agent{Name: "security"}, registry.Get("Bash"), json.RawMessage(`{"command":"rm -rf /"}`))
		if err != nil {
			t.Fatal(err)
		}
		if gr != nil && gr.TripwireTriggered {
			goto builtinInputOK
		}
	}
	t.Fatal("builtin destructive-command guardrail did not trip")
builtinInputOK:
	for _, builtin := range sdkguardrails.BuiltinToolOutputGuardrails() {
		gr, err := builtin.Fn(&agentsdk.RunContext{}, &agentsdk.Agent{Name: "security"}, registry.Get("Bash"), agentsdk.ToolResult{Content: "-----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY-----"})
		if err != nil {
			t.Fatal(err)
		}
		if gr != nil && gr.TripwireTriggered {
			goto builtinOutputOK
		}
	}
	t.Fatal("builtin secret-output guardrail did not trip")
builtinOutputOK:
	runtimePolicy := sdkpolicy.RuntimePolicy{}.Normalize()
	if runtimePolicy.PermissionMode != sdkpolicy.PermissionModeWorkspaceWrite || !runtimePolicy.EnableMCP {
		t.Fatalf("default runtime policy = %+v", runtimePolicy)
	}
	if sdkpolicy.NewToolPolicy(sdkpolicy.RuntimePolicy{PermissionMode: sdkpolicy.PermissionModeReadOnly}).AllowsWriteTools() {
		t.Fatal("read-only policy should not allow write tools")
	}
}

func TestSDKSecurityRegressionCoverage(t *testing.T) {
	ctx := context.Background()

	t.Run("input guardrails block before model calls", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{{
			Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "should not run"}}},
		}}}
		_, err := agentsdk.NewRunnerWithModel(model).Run(ctx, &agentsdk.Agent{
			Name: "pre-model-guarded",
			InputGuardrails: []agentsdk.InputGuardrail{{
				Name: "block-input",
				Fn: func(*agentsdk.RunContext, *agentsdk.Agent, []agentsdk.RunItem) (*agentsdk.GuardrailResult, error) {
					return &agentsdk.GuardrailResult{TripwireTriggered: true, Output: "blocked"}, nil
				},
			}},
		}, []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "blocked input"}}}, agentsdk.RunConfig{})
		var tripwire *agentsdk.InputGuardrailTripwireTriggered
		if !errors.As(err, &tripwire) {
			t.Fatalf("err = %T %v, want InputGuardrailTripwireTriggered", err, err)
		}
		if len(model.requests) != 0 {
			t.Fatalf("model requests = %d, want 0 before input guardrail tripwire", len(model.requests))
		}
	})

	t.Run("tool-final outputs run output guardrails", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{{
			Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
				ID:    "call-final",
				Name:  "final_tool",
				Input: json.RawMessage(`{}`),
			}}},
		}}}
		tool := &agentsdk.FunctionTool{
			ToolName:        "final_tool",
			ToolDescription: "returns final text",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				return "secret final output", nil
			},
		}
		_, err := agentsdk.NewRunnerWithModel(model).Run(ctx, &agentsdk.Agent{
			Name:               "tool-final-guarded",
			Tools:              []agentsdk.Tool{tool},
			StopAtTools:        &agentsdk.StopAtTools{ToolNames: []string{"final_tool"}},
			ToolsToFinalOutput: &agentsdk.ToolsToFinalOutputResult{IsFinalOutput: true},
			OutputGuardrails: []agentsdk.OutputGuardrail{{
				Name: "block-secret-final",
				Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, output any) (*agentsdk.GuardrailResult, error) {
					return &agentsdk.GuardrailResult{TripwireTriggered: strings.Contains(fmt.Sprint(output), "secret")}, nil
				},
			}},
		}, nil, agentsdk.RunConfig{})
		var tripwire *agentsdk.OutputGuardrailTripwireTriggered
		if !errors.As(err, &tripwire) {
			t.Fatalf("err = %T %v, want OutputGuardrailTripwireTriggered", err, err)
		}
	})

	t.Run("approved tools still use runner guardrails", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
					ID:    "mutate-call",
					Name:  "mutate_guarded",
					Input: json.RawMessage(`{"value":"secret"}`),
				}}},
			},
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "guardrail handled"}}},
			},
		}}
		var executed bool
		tool := &agentsdk.FunctionTool{
			ToolName:        "mutate_guarded",
			ToolDescription: "mutates guarded state",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				executed = true
				return "mutated", nil
			},
		}
		result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner:       agentsdk.NewRunnerWithModel(model),
			Agent:        &agentsdk.Agent{Name: "approval-guarded", Tools: []agentsdk.Tool{tool}},
			ApprovalGate: integrationApprovalGate{approved: true},
			RunConfig: agentsdk.RunConfig{
				MaxTurns:   2,
				ToolPolicy: &agentsdk.ToolPolicy{ApprovalRequired: true},
				ToolInputGuardrails: []agentsdk.ToolInputGuardrail{{
					Name: "block-secret-approved-input",
					Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, input json.RawMessage) (*agentsdk.GuardrailResult, error) {
						return &agentsdk.GuardrailResult{TripwireTriggered: strings.Contains(string(input), "secret")}, nil
					},
				}},
			},
		}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if executed {
			t.Fatal("approved guarded tool executed after input guardrail tripwire")
		}
		if normalizeText(result.FinalText()) != "guardrail handled" {
			t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
		}
		if result == nil || !hasRunItemType(result.NewItems, agentsdk.RunItemToolApproval) || !runItemsContainToolOutput(result.NewItems, "tool input guardrail") {
			t.Fatalf("approval resume items = %#v, want approval and guarded tool output", result)
		}
	})

	t.Run("approved tools still use runner timeout policy", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{
			{Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
				ID:    "slow-call",
				Name:  "slow_mutate",
				Input: json.RawMessage(`{}`),
			}}}},
			{Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "timeout handled"}}}},
		}}
		tool := &agentsdk.FunctionTool{
			ToolName:        "slow_mutate",
			ToolDescription: "blocks until canceled",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(ctx context.Context, _ json.RawMessage) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		}
		start := time.Now()
		result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner:       agentsdk.NewRunnerWithModel(model),
			Agent:        &agentsdk.Agent{Name: "approval-timeout", Tools: []agentsdk.Tool{tool}},
			ApprovalGate: integrationApprovalGate{approved: true},
			RunConfig:    agentsdk.RunConfig{MaxTurns: 3, ToolPolicy: &agentsdk.ToolPolicy{ApprovalRequired: true, DefaultTimeout: 1}},
		}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(start); elapsed > 3*time.Second {
			t.Fatalf("approved tool elapsed = %s, want timeout policy to interrupt", elapsed)
		}
		if normalizeText(result.FinalText()) != "timeout handled" || !runItemsContainToolOutput(result.NewItems, "timed out") {
			t.Fatalf("timeout result text=%q items=%#v", result.FinalText(), result.NewItems)
		}
	})

	t.Run("approved tools still use runner output guardrails", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
					ID:    "mutate-output-call",
					Name:  "mutate_output_guarded",
					Input: json.RawMessage(`{"value":"x"}`),
				}}},
			},
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "guardrail handled"}}},
			},
		}}
		var executed bool
		tool := &agentsdk.FunctionTool{
			ToolName:        "mutate_output_guarded",
			ToolDescription: "returns guarded output",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				executed = true
				return "secret output", nil
			},
		}
		result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner:       agentsdk.NewRunnerWithModel(model),
			Agent:        &agentsdk.Agent{Name: "approval-output-guarded", Tools: []agentsdk.Tool{tool}},
			ApprovalGate: integrationApprovalGate{approved: true},
			RunConfig: agentsdk.RunConfig{
				MaxTurns:   2,
				ToolPolicy: &agentsdk.ToolPolicy{ApprovalRequired: true},
				ToolOutputGuardrails: []agentsdk.ToolOutputGuardrail{{
					Name: "block-secret-approved-output",
					Fn: func(_ *agentsdk.RunContext, _ *agentsdk.Agent, _ agentsdk.Tool, result agentsdk.ToolResult) (*agentsdk.GuardrailResult, error) {
						return &agentsdk.GuardrailResult{TripwireTriggered: strings.Contains(result.Content, "secret")}, nil
					},
				}},
			},
		}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !executed {
			t.Fatal("tool should execute before output guardrail checks approved result")
		}
		if normalizeText(result.FinalText()) != "guardrail handled" {
			t.Fatalf("FinalText() = %q, want guardrail handled", result.FinalText())
		}
		if result == nil || !hasRunItemType(result.NewItems, agentsdk.RunItemToolApproval) || !runItemsContainToolOutput(result.NewItems, "tool output guardrail") {
			t.Fatalf("approval output guardrail items = %#v", result)
		}
	})

	t.Run("chat loop config guardrail rules are enforced", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
					ID:    "config-call",
					Name:  "config_mutate",
					Input: json.RawMessage(`{"value":"secret"}`),
				}}},
			},
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "guardrail handled"}}},
			},
		}}
		var executed bool
		tool := &agentsdk.FunctionTool{
			ToolName:        "config_mutate",
			ToolDescription: "mutates config guarded state",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				executed = true
				return "mutated", nil
			},
		}
		result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner: agentsdk.NewRunnerWithModel(model),
			Agent:  &agentsdk.Agent{Name: "config-guarded", Tools: []agentsdk.Tool{tool}},
			ConfigSource: integrationConfigSource{mode: agentsdk.PermissionModeDangerFullAccess, rules: []agentsdk.GuardrailRule{{
				Name:        "block-secret-config-input",
				Type:        "tool-input",
				ToolPattern: "config_mutate",
				Regex:       "secret",
				Action:      "block",
			}}},
			RunConfig: agentsdk.RunConfig{MaxTurns: 2},
		}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if executed {
			t.Fatal("tool executed despite config guardrail rule")
		}
		if normalizeText(result.FinalText()) != "guardrail handled" || !runItemsContainToolOutput(result.NewItems, "tool input guardrail") {
			t.Fatalf("config guardrail result = %#v, want guarded tool output and final", result)
		}
	})

	t.Run("chat loop config output guardrail rules are enforced", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemToolCall, ToolCall: &agentsdk.ToolCallData{
					ID:    "config-output-call",
					Name:  "config_output_mutate",
					Input: json.RawMessage(`{"value":"x"}`),
				}}},
			},
			{
				Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "guardrail handled"}}},
			},
		}}
		var executed bool
		tool := &agentsdk.FunctionTool{
			ToolName:        "config_output_mutate",
			ToolDescription: "returns config guarded output",
			Schema:          json.RawMessage(`{"type":"object"}`),
			Fn: func(context.Context, json.RawMessage) (string, error) {
				executed = true
				return "secret output", nil
			},
		}
		result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner: agentsdk.NewRunnerWithModel(model),
			Agent:  &agentsdk.Agent{Name: "config-output-guarded", Tools: []agentsdk.Tool{tool}},
			ConfigSource: integrationConfigSource{mode: agentsdk.PermissionModeDangerFullAccess, rules: []agentsdk.GuardrailRule{{
				Name:        "block-secret-config-output",
				Type:        "tool-output",
				ToolPattern: "config_output_mutate",
				Regex:       "secret",
				Action:      "block",
			}}},
			RunConfig: agentsdk.RunConfig{MaxTurns: 2},
		}).Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !executed {
			t.Fatal("tool should execute before config output guardrail checks result")
		}
		if normalizeText(result.FinalText()) != "guardrail handled" || !runItemsContainToolOutput(result.NewItems, "tool output guardrail") {
			t.Fatalf("config output guardrail result = %#v, want guarded tool output and final", result)
		}
	})

	t.Run("invalid chat loop config guardrail rules fail closed before model calls", func(t *testing.T) {
		model := &scriptedIntegrationModel{responses: []*agentsdk.ModelResponse{{
			Items: []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "should not run"}}},
		}}}
		_, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
			Runner: agentsdk.NewRunnerWithModel(model),
			Agent:  &agentsdk.Agent{Name: "invalid-config-guardrail"},
			ConfigSource: integrationConfigSource{mode: agentsdk.PermissionModeDangerFullAccess, rules: []agentsdk.GuardrailRule{{
				Name:   "bad-regex",
				Type:   "tool-input",
				Regex:  "[",
				Action: "block",
			}}},
			RunConfig: agentsdk.RunConfig{MaxTurns: 1},
		}).Run(ctx)
		if err == nil || !strings.Contains(err.Error(), "compile guardrail rules") {
			t.Fatalf("err = %v, want compile guardrail rules failure", err)
		}
		if len(model.requests) != 0 {
			t.Fatalf("model requests = %d, want 0 after invalid config guardrail", len(model.requests))
		}
	})

	t.Run("tool workspace browser and vision security boundaries", func(t *testing.T) {
		root := t.TempDir()
		workDir := filepath.Join(root, "workspace")
		outside := filepath.Join(root, "outside.txt")
		must(t, os.Mkdir(workDir, 0o755))
		must(t, os.WriteFile(outside, []byte("before"), 0o644))
		if err := os.Symlink(outside, filepath.Join(workDir, "link.txt")); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}

		registry := sdktools.NewRegistry(
			workDir,
			sdktools.WithPermissionMode(sdkpolicy.PermissionModeWorkspaceWrite),
			sdktools.WithBrowserTools(),
			sdktools.WithVisionTools(func(context.Context, []byte, string, string) (string, error) { return "vision ok", nil }),
		)
		writeResult, err := registry.Get("Write").Execute(ctx, json.RawMessage(`{"file_path":"link.txt","content":"after"}`), workDir)
		if err != nil || !writeResult.IsError {
			t.Fatalf("Write final symlink result=%+v err=%v", writeResult, err)
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "before" {
			t.Fatalf("outside content after symlink Write = %q err=%v", data, err)
		}
		editResult, err := registry.Get("Edit").Execute(ctx, json.RawMessage(`{"file_path":"link.txt","old_string":"before","new_string":"after"}`), workDir)
		if err != nil || !editResult.IsError {
			t.Fatalf("Edit final symlink result=%+v err=%v", editResult, err)
		}
		if data, err := os.ReadFile(outside); err != nil || string(data) != "before" {
			t.Fatalf("outside content after symlink Edit = %q err=%v", data, err)
		}

		outsideImage := filepath.Join(root, "outside.png")
		must(t, os.WriteFile(outsideImage, tinyPNG(), 0o644))
		visionFileResult, err := registry.Get("AnalyzeImage").Execute(ctx, json.RawMessage(fmt.Sprintf(`{"image_path":%q,"prompt":"inspect"}`, outsideImage)), workDir)
		if err != nil || !visionFileResult.IsError || !strings.Contains(visionFileResult.Content, "outside the workspace root") {
			t.Fatalf("AnalyzeImage file escape result=%+v err=%v", visionFileResult, err)
		}
		visionURLResult, err := registry.Get("AnalyzeImage").Execute(ctx, json.RawMessage(`{"url":"http://127.0.0.1:1/pixel.png","prompt":"inspect"}`), workDir)
		if err != nil || !visionURLResult.IsError || !strings.Contains(strings.ToLower(visionURLResult.Content), "private or local") {
			t.Fatalf("AnalyzeImage SSRF result=%+v err=%v", visionURLResult, err)
		}
		webResult, err := registry.Get("WebFetch").Execute(ctx, json.RawMessage(`{"url":"http://127.0.0.1:1/"}`), workDir)
		if err != nil || !webResult.IsError || !strings.Contains(strings.ToLower(webResult.Content), "private or local") {
			t.Fatalf("WebFetch SSRF result=%+v err=%v", webResult, err)
		}

		realDir := filepath.Join(workDir, "real")
		must(t, os.Mkdir(realDir, 0o755))
		must(t, os.WriteFile(filepath.Join(realDir, "note.txt"), []byte("indexed"), 0o644))
		must(t, os.Symlink(realDir, filepath.Join(workDir, "alias")))
		globResult, err := registry.Get("glob").Execute(ctx, json.RawMessage(`{"path":"alias","pattern":"*.txt"}`), workDir)
		if err != nil || globResult.IsError || !strings.Contains(globResult.Content, "real/note.txt") || strings.Contains(globResult.Content, "..") {
			t.Fatalf("glob canonical relative result=%+v err=%v", globResult, err)
		}

		readOnlyRegistry := sdktools.NewRegistry(workDir, sdktools.WithReadOnlyTools(), sdktools.WithBrowserTools())
		readOnlyBrowser := readOnlyRegistry.Get("Browser")
		if readOnlyBrowser == nil {
			t.Fatalf("read-only registry missing Browser; names=%v", readOnlyRegistry.Names())
		}
		if !readOnlyBrowser.IsReadOnly() || strings.Contains(string(readOnlyBrowser.InputSchema()), "screenshot") {
			t.Fatalf("read-only Browser = %#v schema=%s", readOnlyBrowser, readOnlyBrowser.InputSchema())
		}
		browserResult, err := readOnlyBrowser.Execute(ctx, json.RawMessage(`{"action":"screenshot","url":"https://93.184.216.34/","output_path":"shot.png"}`), workDir)
		if err != nil || !browserResult.IsError || !strings.Contains(browserResult.Content, "workspace-write access") {
			t.Fatalf("read-only Browser screenshot result=%+v err=%v", browserResult, err)
		}
	})

	t.Run("mode gates and transitions fail closed", func(t *testing.T) {
		for _, gateName := range []string{sdkmode.GatePrerequisiteArtifact, sdkmode.GateSafety, "unknown_gate"} {
			gate := sdkmode.EvaluateGates([]string{gateName}, sdkmode.GateContext{})
			if gate == nil || gate.Passed || gate.DenyCode != sdkmode.DenyGateFailed {
				t.Fatalf("gate %q = %+v, want fail-closed denial", gateName, gate)
			}
		}
		current := &sdkmode.TemplateSpec{
			Name:        "plan",
			Transitions: []sdkmode.Transition{{From: "plan", To: "review"}},
		}
		if result := sdkmode.Evaluate(current, &sdkmode.TemplateSpec{Name: "ship"}, sdkmode.EvaluateOpts{ActorRole: sdkmode.RoleSystem}); result.Result != sdkmode.ResultDenied || result.DenyCode != sdkmode.DenyEdgeNotFound {
			t.Fatalf("missing transition result = %+v", result)
		}
		if result := sdkmode.Evaluate(&sdkmode.TemplateSpec{
			Name:        "plan",
			Transitions: []sdkmode.Transition{{From: "plan", To: "ship", Gates: []string{sdkmode.GateSafety}}},
		}, &sdkmode.TemplateSpec{Name: "ship"}, sdkmode.EvaluateOpts{ActorRole: sdkmode.RoleSystem}); result.Result != sdkmode.ResultDenied || result.DenyCode != sdkmode.DenyGateFailed {
			t.Fatalf("safety transition result = %+v", result)
		}
	})
}

func TestLiveOpenAIOAuthHostConfigMemoryEventsMCPTracesSandboxCompactionCostsAndWorkflowLogic(t *testing.T) {
	ctx := context.Background()
	runner, model := liveOpenAIRunner(t)
	processor := &recordingTracingProcessor{}
	liveResult, err := runner.Run(ctx, &agentsdk.Agent{
		Name:         "observed-live",
		Model:        model,
		Instructions: "Reply with exactly: observed live ok",
	}, []agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "Say the observed phrase."}}}, agentsdk.RunConfig{
		MaxTurns:         1,
		ModelSettings:    agentsdk.ModelSettings{MaxTokens: 96, ReasoningEffort: string(agentsdk.ReasoningLow)},
		TracingProcessor: processor,
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalizeText(liveResult.FinalText()) != "observed live ok" || processor.traceStarts == 0 || len(processor.spanStarts) == 0 {
		t.Fatalf("observed text=%q processor=%+v", liveResult.FinalText(), processor)
	}

	root := t.TempDir()
	must(t, os.MkdirAll(filepath.Join(root, "modes"), 0o755))
	must(t, os.MkdirAll(filepath.Join(root, "agents"), 0o755))
	must(t, os.WriteFile(filepath.Join(root, "modes", "integration.yaml"), []byte(`apiVersion: platform.gratefulagents.dev/v1alpha1
kind: ModeTemplate
metadata:
  name: integration
spec:
  name: integration
  displayName: Integration
  phases:
    - id: discover
      readOnly: true
    - id: ship
  instructions: |
    Work through the integration test.
`), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "agents", "researcher.md"), []byte(`---
name: researcher
description: Finds facts.
tool_access: read-only
---
Find facts carefully.
`), 0o644))
	source := fileconfig.New(root, t.TempDir(), fileconfig.WithActiveMode("integration"), fileconfig.WithActivePhase("discover"))
	snapshot, err := source.ModeSnapshot(ctx)
	if err != nil || snapshot == nil || snapshot.DisplayName != "Integration" {
		t.Fatalf("snapshot = %+v err=%v", snapshot, err)
	}
	if mode, err := source.PermissionMode(ctx); err != nil || mode != agentsdk.PermissionModeReadOnly {
		t.Fatalf("permission mode = %q err=%v", mode, err)
	}
	roles, err := source.RoleCatalog(ctx)
	if err != nil || len(roles) != 1 || roles[0].Name != "researcher" {
		t.Fatalf("roles = %+v err=%v", roles, err)
	}

	workingState := agentsdk.WorkingState{
		Goal:                 "Verify OAuth integration coverage",
		CurrentMode:          "integration",
		CurrentPhase:         "discover",
		CurrentStep:          "security checks",
		LastUserMessage:      "make it exhaustive",
		LastAssistantSummary: "moved suite under test/integration",
		RecentTurnSummaries:  []string{"runtime checks passed", "tool checks pending"},
	}
	workingStateContext := agentsdk.BuildWorkingStateContext(workingState)
	if !strings.Contains(workingStateContext, "Durable Working State") || !strings.Contains(workingStateContext, "security checks") {
		t.Fatalf("working state context = %q", workingStateContext)
	}
	tail := agentsdk.BuildConversationTail([]agentsdk.ConversationMessage{
		{ID: 1, Role: "user", Content: "below floor"},
		{ID: 2, Role: "assistant", Content: "prior result"},
		{ID: 3, Role: "user", Content: "current request"},
	}, agentsdk.WorkingState{HistoryFloorMessageID: 1}, 3, 8)
	if len(tail) != 1 || tail[0].Agent == nil || tail[0].Agent.Name != "assistant-summary" {
		t.Fatalf("conversation tail = %+v", tail)
	}
	msg, ok, skipCursor, immediate := agentsdk.SelectNextUserMessage([]agentsdk.UserMessage{
		{ID: 10, Content: "", Mode: agentsdk.UserMessageModeImmediate},
		{ID: 11, Content: "urgent", Mode: agentsdk.UserMessageModeImmediate},
		{ID: 12, Content: "queued", Mode: agentsdk.UserMessageModeEnqueue},
	}, map[int64]struct{}{})
	if !ok || !immediate || msg.ID != 11 || skipCursor != 10 {
		t.Fatalf("selected message=%+v ok=%v skip=%d immediate=%v", msg, ok, skipCursor, immediate)
	}
	immediateItems, lastCursor := agentsdk.CollectImmediateRunItems([]agentsdk.UserMessage{{ID: 20, Content: "now", Mode: agentsdk.UserMessageModeImmediate}}, map[int64]struct{}{})
	if len(immediateItems) != 1 || lastCursor != 20 || immediateItems[0].Message.Text != "now" {
		t.Fatalf("immediate items=%+v cursor=%d", immediateItems, lastCursor)
	}
	if !agentsdk.IsSessionModeSlashCommand("/mode integration") || !agentsdk.ValidSessionModeTransition(agentsdk.SessionModePlan, agentsdk.SessionModeChat) {
		t.Fatal("session mode command/transition helpers failed")
	}
	if goal := agentsdk.DeriveWorkingStateGoal("approve", "ship the integration suite"); goal != "ship the integration suite" {
		t.Fatalf("derived goal = %q", goal)
	}

	auto := &agentsdk.AutoTracker{}
	for i := 0; i < 3; i++ {
		auto.Update([]agentsdk.RunItem{{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: "no tools"}}})
	}
	if breaker := auto.CheckCircuitBreakers(); !breaker.Tripped || !strings.Contains(breaker.Reason, "no tool") {
		t.Fatalf("autoloop breaker = %+v", breaker)
	}
	if nudge := agentsdk.BuildSmartNudge(auto, "discover"); !strings.Contains(nudge, "tool") {
		t.Fatalf("smart nudge = %q", nudge)
	}

	memories := sdkmemory.NewInMemoryStore()
	stored, err := memories.Store(ctx, "repo", "Prefer live OAuth SDK tests", []string{"testing"}, "run-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	found, err := memories.Search(ctx, "repo", "OAuth", []string{"testing"}, 5)
	if err != nil || len(found) != 1 || found[0].ID != stored.ID {
		t.Fatalf("memory search = %+v err=%v", found, err)
	}
	vector, err := (&sdkmemory.NoopEmbedder{Dimension: 3}).Embed(ctx, "anything")
	if err != nil || sdkmemory.VectorLiteral(vector) != "[0,0,0]" {
		t.Fatalf("noop embedding=%v err=%v", vector, err)
	}

	var typedEvents []sdkevents.Event
	writer := sdkevents.NewLineWriter(sdkevents.SinkFunc(func(event sdkevents.Event) {
		typedEvents = append(typedEvents, event)
	}))
	stream := agentsdk.NewSessionEventStream(writer, agentsdk.SessionEventStreamOptions{
		Session: 7,
		Phase:   "discover",
		SystemInit: &agentsdk.SessionSystemInit{
			Model:          model,
			PermissionMode: "read-only",
			Cwd:            root,
			MaxTurns:       2,
			Tools:          []string{"lookup"},
			MCPServers:     []string{"notes"},
		},
	})
	stream.EmitText("hello", "agent")
	stream.EmitToolStart("lookup", "call-1", "parent-call", "lookup sdk", "agent", `{"query":"sdk"}`)
	stream.EmitToolEnd("lookup", "call-1", "parent-call", false, "agent", "found", 5)
	if !hasEventKind(typedEvents, sdkevents.KindLog) || !hasEventKind(typedEvents, sdkevents.KindChildTool) {
		t.Fatalf("typed events = %+v", typedEvents)
	}

	artifactStore := &integrationArtifactStore{}
	sessionID := uuid.New()
	planTools := sdksignal.PlanTools(artifactStore, sessionID)
	savePlan := toolByName(t, planTools, "save_plan")
	getPlan := toolByName(t, planTools, "get_plan")
	savePlanResult, err := savePlan.Execute(ctx, json.RawMessage(`{"plan":"1. Verify live OAuth\n2. Verify security","summary":"Verify OAuth and security"}`), root)
	if err != nil || savePlanResult.IsError {
		t.Fatalf("save_plan result=%+v err=%v", savePlanResult, err)
	}
	getPlanResult, err := getPlan.Execute(ctx, json.RawMessage(`{}`), root)
	if err != nil || getPlanResult.IsError || !strings.Contains(getPlanResult.Content, "Verify live OAuth") {
		t.Fatalf("get_plan result=%+v err=%v", getPlanResult, err)
	}
	finishSink := &integrationFinishSink{}
	finishResult, err := (&sdksignal.FinishTool{Sink: finishSink}).Execute(ctx, json.RawMessage(`{"summary":"done"}`), root)
	if err != nil || finishResult.IsError || !finishResult.ShouldPause || finishSink.summary != "done" {
		t.Fatalf("finish result=%+v sink=%+v err=%v", finishResult, finishSink, err)
	}
	phaseSink := &integrationPhaseSink{}
	setPhaseResult, err := (&sdksignal.SetPhaseTool{
		Phases:       []sdksignal.PhaseOption{{ID: "discover", ReadOnly: true}, {ID: "ship", RequiresApproval: true}},
		CurrentPhase: "discover",
		Sink:         phaseSink,
	}).Execute(ctx, json.RawMessage(`{"phase":"ship"}`), root)
	if err != nil || setPhaseResult.IsError || phaseSink.phase != "ship" || !strings.Contains(setPhaseResult.Content, "requires_approval") {
		t.Fatalf("set_phase result=%+v sink=%+v err=%v", setPhaseResult, phaseSink, err)
	}

	manager := &integrationMCPManager{}
	mcpTools := sdkmcp.BuildTools(manager)
	if len(mcpTools) != 1 || mcpTools[0].Name() != "mcp__notes__lookup" {
		t.Fatalf("mcp tools = %+v", mcpTools)
	}
	mcpResult, err := mcpTools[0].Execute(ctx, json.RawMessage(`{"query":"sdk"}`), t.TempDir())
	if err != nil || mcpResult.Content != "note found" || manager.args["query"] != "sdk" {
		t.Fatalf("mcp result=%+v args=%#v err=%v", mcpResult, manager.args, err)
	}
	if !strings.Contains(sdkmcp.BlockedToolMessage("notes", "lookup", true), "RequestMCPBreakGlass") {
		t.Fatal("break-glass prompt missing tool name")
	}

	sandboxResult, err := sdksandbox.DefaultWithConfig(sdksandbox.Config{Mode: "disabled"}).Run(ctx, sdksandbox.Request{
		Argv:           []string{"sh", "-c", "printf sandboxed"},
		WorkDir:        t.TempDir(),
		PermissionMode: sdkpolicy.PermissionModeWorkspaceWrite,
		Timeout:        5 * time.Second,
	})
	if err != nil || strings.TrimSpace(sandboxResult.Output) != "sandboxed" || sandboxResult.ExitCode != 0 {
		t.Fatalf("sandbox result=%+v err=%v", sandboxResult, err)
	}

	// Disabled-mode sandbox must refuse ReadOnly requests rather than silently
	// downgrading enforcement to a non-isolating LocalExecutor.
	if _, err := sdksandbox.DefaultWithConfig(sdksandbox.Config{Mode: "disabled"}).Run(ctx, sdksandbox.Request{
		Argv:           []string{"sh", "-c", "printf sandboxed"},
		WorkDir:        t.TempDir(),
		PermissionMode: sdkpolicy.PermissionModeReadOnly,
		Timeout:        5 * time.Second,
	}); err == nil {
		t.Fatal("disabled-mode sandbox silently accepted ReadOnly request; want refusal")
	}

	traceRoot := t.TempDir()
	store, err := sdktracestore.NewFilesystemTraceStore(traceRoot)
	if err != nil {
		t.Fatal(err)
	}
	runID := "oauth-integration-run"
	_, err = store.CreateRunDir(runID, sdktracestore.RunMetadata{
		RunID:       runID,
		CandidateID: "candidate-a",
		Model:       model,
		StartedAt:   time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	must(t, store.AppendTrace(runID, "llm_calls", []byte(`{"model":"`+model+`"}`)))
	must(t, store.WriteScore(runID, sdktracestore.Score{
		TaskID:      "task-1",
		CandidateID: "candidate-a",
		Success:     true,
		Metrics:     sdktracestore.ScoreMetrics{Accuracy: 1, TokensUsed: 20, ToolCalls: 1},
	}))
	runs, err := store.ListRuns(sdktracestore.RunFilter{CandidateID: "candidate-a"})
	if err != nil || len(runs) != 1 || runs[0].RunID != runID {
		t.Fatalf("trace runs=%+v err=%v", runs, err)
	}

	var history []agentsdk.RunItem
	for i := 0; i < 18; i++ {
		history = append(history, agentsdk.RunItem{Type: agentsdk.RunItemMessage, Message: &agentsdk.MessageOutput{Text: strings.Repeat("important context ", 20)}})
	}
	compacted, before, after, ok, reason := agentsdk.MaybeCompactRunItems(history, agentsdk.CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               100,
		TargetTokens:                60,
		PreserveRecentItems:         3,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          2,
	})
	if !ok || len(compacted) >= len(history) || after >= before {
		t.Fatalf("compaction ok=%v reason=%s len=%d->%d tokens=%d->%d", ok, reason, len(history), len(compacted), before, after)
	}
	if !strings.Contains(agentsdk.ExtractCompactionSummary(compacted), "[COMPACTED HISTORY SUMMARY]") {
		t.Fatal("compaction summary missing")
	}
	usage := agentsdk.Usage{Requests: 1, InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 100}
	if openAICost, known := sdkopenai.EstimateCost(model, usage); !known || openAICost <= 0 {
		t.Fatalf("OpenAI cost = %f known=%v", openAICost, known)
	}
	if anthropicCost := sdkanthropic.CalculateCost("claude-haiku-4-5", usage); anthropicCost <= 0 {
		t.Fatalf("Anthropic cost = %f", anthropicCost)
	}
}

type liveOAuthConfig struct {
	authPath string
	baseURL  string
}

func liveOpenAIRunner(t *testing.T) (*agentsdk.Runner, string) {
	t.Helper()
	cfg := liveOpenAIConfig(t)
	session := liveOAuthSession(t, cfg.authPath)
	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		BaseURL:     cfg.baseURL,
		AuthMode:    sdkopenai.AuthModeOAuth,
		AuthSession: session,
	})
	return agentsdk.NewRunnerWithProvider(provider), liveOpenAIModel
}

func liveOpenAIConfig(t *testing.T) liveOAuthConfig {
	t.Helper()
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GRATEFUL_LIVE_TESTS")), "skip") {
		t.Skip("GRATEFUL_LIVE_TESTS=skip")
	}
	authPath := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_AUTH_JSON_PATH"))
	if authPath == "" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".codex", "auth.json")
			if _, err := os.Stat(candidate); err == nil {
				authPath = candidate
			}
		}
	}
	if authPath == "" {
		t.Skip("set OPENAI_OAUTH_AUTH_JSON_PATH or provide $HOME/.codex/auth.json to run live OpenAI OAuth SDK integration tests")
	}
	if _, err := os.Stat(authPath); err != nil {
		t.Skipf("OAuth auth JSON not available at %s: %v", authPath, err)
	}
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	return liveOAuthConfig{authPath: authPath, baseURL: baseURL}
}

func liveOAuthSession(t *testing.T, authPath string) *sdkopenai.AuthSession {
	t.Helper()
	if accountID := strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID")); accountID != "" {
		authJSON, err := os.ReadFile(authPath)
		if err != nil {
			t.Fatal(err)
		}
		session, err := sdkopenai.NewOAuthAuthSessionFromSecretData(authJSON, accountID)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	session, err := sdkopenai.NewOAuthAuthSessionFromFile(authPath, strings.TrimSpace(os.Getenv("OPENAI_OAUTH_ACCOUNT_ID_PATH")))
	if err != nil {
		t.Fatal(err)
	}
	return session
}

type scriptedIntegrationModel struct {
	responses []*agentsdk.ModelResponse
	requests  []agentsdk.ModelRequest
}

func (m *scriptedIntegrationModel) GetResponse(_ context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelResponse, error) {
	m.requests = append(m.requests, req)
	idx := len(m.requests) - 1
	if idx >= len(m.responses) {
		return nil, fmt.Errorf("unexpected model call %d", idx+1)
	}
	resp := *m.responses[idx]
	resp.Items = append([]agentsdk.RunItem(nil), m.responses[idx].Items...)
	return &resp, nil
}

func (m *scriptedIntegrationModel) StreamResponse(ctx context.Context, req agentsdk.ModelRequest) (*agentsdk.ModelStream, error) {
	resp, err := m.GetResponse(ctx, req)
	if err != nil {
		return nil, err
	}
	events := make(chan agentsdk.ModelStreamEvent, len(resp.Items)+1)
	done := make(chan *agentsdk.ModelResponse, 1)
	go func() {
		defer close(events)
		for i := range resp.Items {
			item := resp.Items[i]
			events <- agentsdk.ModelStreamEvent{Type: agentsdk.ModelStreamItemDone, Item: &item}
		}
		events <- agentsdk.ModelStreamEvent{Type: agentsdk.ModelStreamComplete, Response: resp}
		done <- resp
	}()
	return agentsdk.NewModelStream(events, done), nil
}

func (m *scriptedIntegrationModel) GetRetryAdvice(error) *agentsdk.ModelRetryAdvice {
	return &agentsdk.ModelRetryAdvice{ShouldRetry: false}
}

func (m *scriptedIntegrationModel) CalculateCost(agentsdk.Usage) float64 { return 0 }
func (m *scriptedIntegrationModel) Provider() string                     { return "scripted-security" }

type integrationApprovalGate struct {
	approved bool
}

func (g integrationApprovalGate) ApproveTool(context.Context, agentsdk.ToolApprovalRequest) (bool, string, error) {
	if g.approved {
		return true, "", nil
	}
	return false, "denied", nil
}

type integrationConfigSource struct {
	mode  agentsdk.PermissionMode
	rules []agentsdk.GuardrailRule
}

func (s integrationConfigSource) PermissionMode(context.Context) (agentsdk.PermissionMode, error) {
	if s.mode == "" {
		return agentsdk.PermissionModeWorkspaceWrite, nil
	}
	return s.mode, nil
}

func (integrationConfigSource) ModeSnapshot(context.Context) (*sdkmode.TemplateSpec, error) {
	return nil, nil
}

func (s integrationConfigSource) GuardrailRules(context.Context) ([]agentsdk.GuardrailRule, error) {
	return append([]agentsdk.GuardrailRule(nil), s.rules...), nil
}

func (integrationConfigSource) RoleCatalog(context.Context) (agentsdk.RoleCatalog, error) {
	return nil, nil
}

func (integrationConfigSource) MCPServers(context.Context) (map[string]agentsdk.MCPServerConfig, error) {
	return nil, nil
}

func (integrationConfigSource) PhaseDirective(context.Context) (string, error) {
	return "", nil
}

func (integrationConfigSource) HandoffHistory(context.Context) ([]agentsdk.RunItem, error) {
	return nil, nil
}

type integrationSession struct {
	messages []agentsdk.UserMessage
	state    agentsdk.WorkingState
	items    []agentsdk.RunItem
}

func (s *integrationSession) LoadMessages(context.Context, agentsdk.Cursor, int) ([]agentsdk.UserMessage, agentsdk.Cursor, error) {
	return append([]agentsdk.UserMessage(nil), s.messages...), agentsdk.Cursor{}, nil
}

func (s *integrationSession) AppendRunItems(_ context.Context, items []agentsdk.RunItem) error {
	s.items = append(s.items, items...)
	return nil
}

func (s *integrationSession) WorkingState(context.Context) (agentsdk.WorkingState, error) {
	return s.state, nil
}

type integrationMCPManager struct {
	args map[string]any
}

func (m *integrationMCPManager) ToolDescriptors() []sdkmcp.ToolDescriptor {
	return []sdkmcp.ToolDescriptor{{
		QualifiedName: "mcp__notes__lookup",
		ServerName:    "notes",
		ToolName:      "lookup",
		Description:   "Lookup notes",
		ReadOnly:      true,
	}}
}

func (m *integrationMCPManager) ConnectedServerNames() []string { return []string{"notes"} }
func (m *integrationMCPManager) HasResources() bool             { return false }
func (m *integrationMCPManager) ListResources(context.Context, string) ([]sdkmcp.ResourceDescriptor, error) {
	return nil, nil
}
func (m *integrationMCPManager) ReadResource(context.Context, string, string) (*mcpsdk.ReadResourceResult, error) {
	return nil, nil
}
func (m *integrationMCPManager) CallTool(_ context.Context, _ string, args map[string]any) (*mcpsdk.CallToolResult, error) {
	m.args = args
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "note found"}}}, nil
}

type recordingRunHooks struct {
	agentStarts int
	agentEnds   int
	handoffs    int
	toolStarts  int
	toolEnds    int
	llmStarts   int
	llmEnds     int
}

func (h *recordingRunHooks) OnAgentStart(*agentsdk.RunContext, *agentsdk.Agent) { h.agentStarts++ }
func (h *recordingRunHooks) OnAgentEnd(*agentsdk.RunContext, *agentsdk.Agent, any) {
	h.agentEnds++
}
func (h *recordingRunHooks) OnHandoff(*agentsdk.RunContext, *agentsdk.Agent, *agentsdk.Agent) {
	h.handoffs++
}
func (h *recordingRunHooks) OnToolStart(*agentsdk.RunContext, *agentsdk.Agent, agentsdk.Tool, agentsdk.ToolCallData) {
	h.toolStarts++
}
func (h *recordingRunHooks) OnToolEnd(*agentsdk.RunContext, *agentsdk.Agent, agentsdk.Tool, agentsdk.ToolCallData, agentsdk.ToolResult) {
	h.toolEnds++
}
func (h *recordingRunHooks) OnLLMStart(*agentsdk.RunContext, *agentsdk.Agent) { h.llmStarts++ }
func (h *recordingRunHooks) OnLLMEnd(*agentsdk.RunContext, *agentsdk.Agent, *agentsdk.ModelResponse) {
	h.llmEnds++
}

type recordingTracingProcessor struct {
	traceStarts int
	traceEnds   int
	spanStarts  []string
	spanEnds    []string
}

func (p *recordingTracingProcessor) OnTraceStart(*agentsdk.Trace) { p.traceStarts++ }
func (p *recordingTracingProcessor) OnTraceEnd(*agentsdk.Trace)   { p.traceEnds++ }
func (p *recordingTracingProcessor) OnSpanStart(span *agentsdk.Span) {
	p.spanStarts = append(p.spanStarts, span.Name)
}
func (p *recordingTracingProcessor) OnSpanEnd(span *agentsdk.Span) {
	p.spanEnds = append(p.spanEnds, span.Name)
}
func (p *recordingTracingProcessor) Flush() {}

func hasEventKind(events []sdkevents.Event, kind sdkevents.Kind) bool {
	for _, event := range events {
		if event.Kind == kind {
			return true
		}
	}
	return false
}

func hasRunItemType(items []agentsdk.RunItem, typ agentsdk.RunItemType) bool {
	for _, item := range items {
		if item.Type == typ {
			return true
		}
	}
	return false
}

func runItemsText(items []agentsdk.RunItem) string {
	var b strings.Builder
	for _, item := range items {
		if item.Message != nil {
			b.WriteString(item.Message.Text)
			b.WriteByte('\n')
		}
		if item.ToolOutput != nil {
			b.WriteString(item.ToolOutput.Content)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func runItemsContainToolOutput(items []agentsdk.RunItem, needle string) bool {
	for _, item := range items {
		if item.ToolOutput != nil && strings.Contains(item.ToolOutput.Content, needle) {
			return true
		}
	}
	return false
}

func toolByName(t *testing.T, tools []agentsdk.Tool, name string) agentsdk.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	var names []string
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	t.Fatalf("tool %q missing; got %v", name, names)
	return nil
}

func tinyPNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53,
		0xde, 0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41,
		0x54, 0x08, 0xd7, 0x63, 0xf8, 0xff, 0xff, 0x3f,
		0x00, 0x05, 0xfe, 0x02, 0xfe, 0xdc, 0xcc, 0x59,
		0xe7, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e,
		0x44, 0xae, 0x42, 0x60, 0x82,
	}
}

type integrationArtifactStore struct {
	artifacts map[uuid.UUID]string
}

func (s *integrationArtifactStore) UpsertArtifact(_ context.Context, sessionID uuid.UUID, kind, content, _, _ string, _ json.RawMessage) (any, error) {
	if kind != "plan" {
		return nil, fmt.Errorf("unexpected artifact kind %q", kind)
	}
	if s.artifacts == nil {
		s.artifacts = make(map[uuid.UUID]string)
	}
	s.artifacts[sessionID] = content
	return struct{}{}, nil
}

func (s *integrationArtifactStore) GetArtifact(_ context.Context, sessionID uuid.UUID, kind string) (*sdksignal.Artifact, error) {
	if kind != "plan" {
		return nil, fmt.Errorf("unexpected artifact kind %q", kind)
	}
	if s.artifacts == nil {
		return nil, sdksignal.ErrArtifactNotFound
	}
	content, ok := s.artifacts[sessionID]
	if !ok {
		return nil, sdksignal.ErrArtifactNotFound
	}
	return &sdksignal.Artifact{Content: content}, nil
}

type integrationFinishSink struct {
	summary string
}

func (s *integrationFinishSink) Finish(_ context.Context, summary string) error {
	s.summary = summary
	return nil
}

type integrationPhaseSink struct {
	phase string
}

func (s *integrationPhaseSink) SetPhase(_ context.Context, phase string) error {
	s.phase = phase
	return nil
}

func normalizeText(text string) string {
	text = strings.TrimSpace(strings.ToLower(text))
	text = strings.Trim(text, ".!`\"'")
	text = strings.TrimSpace(text)
	return text
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
