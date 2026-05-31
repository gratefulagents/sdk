package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestAgentGetInstructions_Static(t *testing.T) {
	a := &Agent{Instructions: "You are a helpful assistant."}
	ctx := newRunContext(context.Background(), RunConfig{})
	got := a.GetInstructions(ctx)
	if got != "You are a helpful assistant." {
		t.Errorf("expected static instructions, got %q", got)
	}
}

func TestAgentGetInstructions_Dynamic(t *testing.T) {
	a := &Agent{
		Instructions: "fallback",
		InstructionsFn: func(ctx *RunContext, agent *Agent) string {
			return "dynamic: " + agent.Name
		},
		Name: "test-agent",
	}
	ctx := newRunContext(context.Background(), RunConfig{})
	got := a.GetInstructions(ctx)
	if got != "dynamic: test-agent" {
		t.Errorf("expected dynamic instructions, got %q", got)
	}
}

func TestAgentGetAllTools(t *testing.T) {
	target := &Agent{Name: "helper", HandoffDescription: "A helper agent"}
	a := &Agent{
		Tools: []Tool{
			&FunctionTool{ToolName: "tool1", ToolDescription: "desc1"},
		},
		Handoffs: []*Handoff{NewHandoff(target)},
	}
	ctx := newRunContext(context.Background(), RunConfig{})
	tools := a.GetAllTools(ctx)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name() != "tool1" {
		t.Errorf("expected tool1, got %s", tools[0].Name())
	}
	if tools[1].Name() != "transfer_to_helper" {
		t.Errorf("expected transfer_to_helper, got %s", tools[1].Name())
	}
}

func TestAgentClone(t *testing.T) {
	a := &Agent{Name: "original", Instructions: "do stuff"}
	b := a.Clone(WithName("clone"), WithInstructions("do other stuff"))
	if b.Name != "clone" {
		t.Errorf("expected clone name, got %q", b.Name)
	}
	if b.Instructions != "do other stuff" {
		t.Errorf("expected overridden instructions")
	}
	if a.Name != "original" {
		t.Error("original should not be modified")
	}
}

func TestRunConfigEffectiveMaxTurns(t *testing.T) {
	c := RunConfig{}
	if c.EffectiveMaxTurns() != DefaultMaxTurns {
		t.Errorf("expected %d, got %d", DefaultMaxTurns, c.EffectiveMaxTurns())
	}
	c.MaxTurns = 50
	if c.EffectiveMaxTurns() != 50 {
		t.Errorf("expected 50, got %d", c.EffectiveMaxTurns())
	}
}

func TestRunConfigEffectiveSubAgentMaxTurns(t *testing.T) {
	c := RunConfig{}
	if c.EffectiveSubAgentMaxTurns() != DefaultSubAgentMaxTurns {
		t.Errorf("expected %d, got %d", DefaultSubAgentMaxTurns, c.EffectiveSubAgentMaxTurns())
	}
	c.MaxTurns = 400
	if c.EffectiveSubAgentMaxTurns() != DefaultSubAgentMaxTurns {
		t.Errorf("expected sub-agent default %d, got %d", DefaultSubAgentMaxTurns, c.EffectiveSubAgentMaxTurns())
	}
	c.SubAgentMaxTurns = 200
	if c.EffectiveSubAgentMaxTurns() != 200 {
		t.Errorf("expected 200, got %d", c.EffectiveSubAgentMaxTurns())
	}
}

func TestRunResultFinalText(t *testing.T) {
	r := &RunResult{FinalOutput: "hello"}
	if r.FinalText() != "hello" {
		t.Errorf("expected hello, got %q", r.FinalText())
	}
	r2 := &RunResult{FinalOutput: 42}
	if r2.FinalText() != "" {
		t.Errorf("expected empty string for non-string output, got %q", r2.FinalText())
	}
}

func TestRunResultIsInterrupted(t *testing.T) {
	r := &RunResult{}
	if r.IsInterrupted() {
		t.Error("should not be interrupted")
	}
	r.Interruption = &Interruption{ToolName: "bash"}
	if !r.IsInterrupted() {
		t.Error("should be interrupted")
	}
}

func TestUsageAddAndTotal(t *testing.T) {
	u := &Usage{InputTokens: 100, OutputTokens: 50}
	u.Add(Usage{InputTokens: 200, OutputTokens: 100, Requests: 1})
	if u.InputTokens != 300 {
		t.Errorf("expected 300 input tokens, got %d", u.InputTokens)
	}
	if u.TotalTokens() != 450 {
		t.Errorf("expected 450 total tokens, got %d", u.TotalTokens())
	}
}

func TestItemHelpersExtractText(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "hello"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "world"}},
	}
	got := Items.ExtractText(items)
	if got != "hello\nworld" {
		t.Errorf("expected 'hello\\nworld', got %q", got)
	}
}

func TestItemHelpersExtractLastText(t *testing.T) {
	items := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "first"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "last"}},
	}
	got := Items.ExtractLastText(items)
	if got != "last" {
		t.Errorf("expected 'last', got %q", got)
	}
}

func TestItemHelpersExtractToolCalls(t *testing.T) {
	items := []RunItem{
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "bash", ID: "1"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "text"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{Name: "read", ID: "2"}},
	}
	calls := Items.ExtractToolCalls(items)
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}
	if calls[0].Name != "bash" || calls[1].Name != "read" {
		t.Errorf("unexpected tool call names: %s, %s", calls[0].Name, calls[1].Name)
	}
}

func TestModelSettingsMerge(t *testing.T) {
	temp := 0.5
	base := ModelSettings{Temperature: &temp, MaxTokens: 1000}
	override := ModelSettings{MaxTokens: 2000, ReasoningEffort: "high", TextVerbosity: "low"}
	merged := base.Merge(override)
	if *merged.Temperature != 0.5 {
		t.Error("temperature should be preserved")
	}
	if merged.MaxTokens != 2000 {
		t.Errorf("expected 2000, got %d", merged.MaxTokens)
	}
	if merged.ReasoningEffort != "high" {
		t.Error("reasoning effort should be overridden")
	}
	if merged.TextVerbosity != "low" {
		t.Error("text verbosity should be overridden")
	}
}

func TestFunctionToolExecute(t *testing.T) {
	tool := &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(ctx context.Context, input json.RawMessage) (string, error) {
			return "echoed: " + string(input), nil
		},
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`"hello"`), "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != `echoed: "hello"` {
		t.Errorf("unexpected result: %q", result.Content)
	}
	if result.IsError {
		t.Error("should not be error")
	}
}

func TestHandoffToTool(t *testing.T) {
	target := &Agent{Name: "expert"}
	h := NewHandoff(target)
	tool := h.ToTool()
	if tool.Name() != "transfer_to_expert" {
		t.Errorf("expected transfer_to_expert, got %s", tool.Name())
	}
	if !tool.IsReadOnly() {
		t.Error("handoff tool should be read-only")
	}
}

func TestNewModel(t *testing.T) {
	mp := NewMultiProvider("openai")
	_, err := mp.GetModel("invalid/model")
	if err == nil {
		t.Error("expected error for invalid provider")
	}
}

func TestStopAtToolsContains(t *testing.T) {
	s := &StopAtTools{ToolNames: []string{"bash", "read"}}
	if !s.Contains("bash") {
		t.Error("should contain bash")
	}
	if s.Contains("write") {
		t.Error("should not contain write")
	}
}

func TestGuardrailTripwireErrors(t *testing.T) {
	e1 := &InputGuardrailTripwireTriggered{GuardrailName: "safety"}
	if e1.Error() != `input guardrail "safety" tripwire triggered` {
		t.Errorf("unexpected error: %s", e1.Error())
	}
	e2 := &OutputGuardrailTripwireTriggered{GuardrailName: "format"}
	if e2.Error() != `output guardrail "format" tripwire triggered` {
		t.Errorf("unexpected error: %s", e2.Error())
	}
}

func TestMaxTurnsExceededError(t *testing.T) {
	e := &MaxTurnsExceeded{MaxTurns: 50}
	if e.Error() != "max turns exceeded: 50" {
		t.Errorf("unexpected error: %s", e.Error())
	}
}

func TestOutputSchemaNew(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	os := NewOutputSchema("test", schema)
	if os.Name != "test" {
		t.Error("unexpected name")
	}
	if !os.Strict {
		t.Error("should default to strict")
	}
}

func TestNoOpHooksCompile(t *testing.T) {
	var rh RunHooks = NoOpRunHooks{}
	rh.OnAgentStart(nil, nil)
	rh.OnAgentEnd(nil, nil, nil)
	rh.OnHandoff(nil, nil, nil)
	rh.OnToolStart(nil, nil, nil, ToolCallData{})
	rh.OnToolEnd(nil, nil, nil, ToolCallData{}, ToolResult{})
	rh.OnLLMStart(nil, nil)
	rh.OnLLMEnd(nil, nil, nil)

	var ah AgentHooks = NoOpAgentHooks{}
	ah.OnStart(nil, nil)
	ah.OnEnd(nil, nil, nil)
	ah.OnHandoff(nil, nil, nil)
	ah.OnToolStart(nil, nil, nil, ToolCallData{})
	ah.OnToolEnd(nil, nil, nil, ToolCallData{}, ToolResult{})
}
