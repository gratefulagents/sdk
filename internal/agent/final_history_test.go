package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// finalHistoryEchoTool returns a simple echo tool for FinalHistory tests.
func finalHistoryEchoTool() *FunctionTool {
	return &FunctionTool{
		ToolName:        "echo",
		ToolDescription: "echoes input",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(_ context.Context, input json.RawMessage) (string, error) {
			return "echoed: " + string(input), nil
		},
	}
}

func TestFinalHistoryFinalOutput(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "final answer"}}}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test"}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "hi"}}}

	result, err := runner.Run(context.Background(), agentDef, input, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(result.FinalHistory); got != 2 {
		t.Fatalf("expected 2 items in FinalHistory, got %d: %+v", got, result.FinalHistory)
	}
	if result.FinalHistory[0].Message == nil || result.FinalHistory[0].Message.Text != "hi" {
		t.Errorf("expected first item to be the user input, got %+v", result.FinalHistory[0])
	}
	last := result.FinalHistory[1]
	if last.Message == nil || last.Message.Text != "final answer" {
		t.Errorf("expected last item to be the final answer, got %+v", last)
	}
}

func TestFinalHistoryToolRoundTrip(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID: "call1", Name: "echo", Input: json.RawMessage(`{"q":1}`),
		}}}},
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test", Tools: []Tool{finalHistoryEchoTool()}}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "use the tool"}}}

	result, err := runner.Run(context.Background(), agentDef, input, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []RunItemType{RunItemMessage, RunItemToolCall, RunItemToolOutput, RunItemMessage}
	if len(result.FinalHistory) != len(wantTypes) {
		t.Fatalf("expected %d items in FinalHistory, got %d: %+v", len(wantTypes), len(result.FinalHistory), result.FinalHistory)
	}
	for i, want := range wantTypes {
		if result.FinalHistory[i].Type != want {
			t.Errorf("FinalHistory[%d]: expected type %v, got %v", i, want, result.FinalHistory[i].Type)
		}
	}
	out := result.FinalHistory[2].ToolOutput
	if out == nil || out.CallID != "call1" || !strings.Contains(out.Content, "echoed:") {
		t.Errorf("expected paired tool output for call1 in FinalHistory, got %+v", result.FinalHistory[2])
	}
}

func TestFinalHistoryPauseToolIncludesPairedOutput(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID: "ask1", Name: "AskUserQuestion", Input: json.RawMessage(`{"question":"?"}`),
		}}}},
	}}
	askTool := &FunctionTool{
		ToolName:        "AskUserQuestion",
		ToolDescription: "asks the user",
		Schema:          json.RawMessage(`{"type":"object"}`),
		Fn: func(context.Context, json.RawMessage) (string, error) {
			return "asked", nil
		},
	}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test", Tools: []Tool{askTool}}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "plan something"}}}

	result, err := runner.Run(context.Background(), agentDef, input, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsInterrupted() {
		t.Fatal("pause tool should not surface as an interruption")
	}
	wantTypes := []RunItemType{RunItemMessage, RunItemToolCall, RunItemToolOutput}
	if len(result.FinalHistory) != len(wantTypes) {
		t.Fatalf("expected %d items in FinalHistory, got %d: %+v", len(wantTypes), len(result.FinalHistory), result.FinalHistory)
	}
	for i, want := range wantTypes {
		if result.FinalHistory[i].Type != want {
			t.Errorf("FinalHistory[%d]: expected type %v, got %v", i, want, result.FinalHistory[i].Type)
		}
	}
	out := result.FinalHistory[2].ToolOutput
	if out == nil || out.CallID != "ask1" {
		t.Errorf("expected paired pause-tool output for ask1, got %+v", result.FinalHistory[2])
	}
}

func TestFinalHistoryStopOnFirstTool(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID: "call1", Name: "echo", Input: json.RawMessage(`{}`),
		}}}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test", Tools: []Tool{finalHistoryEchoTool()}, ToolUseBehavior: StopOnFirstTool}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "go"}}}

	result, err := runner.Run(context.Background(), agentDef, input, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []RunItemType{RunItemMessage, RunItemToolCall, RunItemToolOutput}
	if len(result.FinalHistory) != len(wantTypes) {
		t.Fatalf("expected %d items in FinalHistory, got %d: %+v", len(wantTypes), len(result.FinalHistory), result.FinalHistory)
	}
	for i, want := range wantTypes {
		if result.FinalHistory[i].Type != want {
			t.Errorf("FinalHistory[%d]: expected type %v, got %v", i, want, result.FinalHistory[i].Type)
		}
	}
}

func TestFinalHistoryInterruptionEndsWithPendingApproval(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemToolCall, ToolCall: &ToolCallData{
			ID: "call1", Name: "echo", Input: json.RawMessage(`{}`),
		}}}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test", Tools: []Tool{finalHistoryEchoTool()}}
	input := []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "go"}}}
	cfg := RunConfig{ToolPolicy: &ToolPolicy{ApprovalRequired: true}}

	result, err := runner.Run(context.Background(), agentDef, input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsInterrupted() {
		t.Fatal("expected approval interruption")
	}
	if len(result.FinalHistory) == 0 {
		t.Fatal("expected non-empty FinalHistory on interruption")
	}
	last := result.FinalHistory[len(result.FinalHistory)-1]
	if last.Type != RunItemToolApproval {
		t.Errorf("expected FinalHistory to end with the pending approval, got %v", last.Type)
	}
}

func TestFinalHistoryReflectsCompaction(t *testing.T) {
	var input []RunItem
	input = append(input, RunItem{Type: RunItemMessage, Message: &MessageOutput{Text: "original task: build the thing"}})
	filler := strings.Repeat("lorem ipsum dolor sit amet consectetur ", 30)
	for i := 0; i < 30; i++ {
		input = append(input, RunItem{Type: RunItemMessage, Message: &MessageOutput{
			Text: fmt.Sprintf("note %d: %s", i, filler),
		}})
	}
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}}}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test"}
	cfg := RunConfig{CompactionConfig: CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               500,
		TargetTokens:                250,
		PreserveRecentItems:         2,
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          4,
		UseLLMSummary:               false,
	}}

	result, err := runner.Run(context.Background(), agentDef, input, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FinalHistory) >= len(input) {
		t.Fatalf("expected compacted FinalHistory shorter than input (%d items), got %d", len(input), len(result.FinalHistory))
	}
	if ExtractCompactionSummary(result.FinalHistory) == "" {
		t.Error("expected FinalHistory to carry the compaction summary")
	}
	last := result.FinalHistory[len(result.FinalHistory)-1]
	if last.Message == nil || last.Message.Text != "done" {
		t.Errorf("expected FinalHistory to end with the final answer, got %+v", last)
	}
	// Replaying FinalHistory must not resend the uncompacted transcript.
	if got, orig := estimateRunItemsTokens(result.FinalHistory), estimateRunItemsTokens(input); got >= orig {
		t.Errorf("expected FinalHistory (%d tokens) to be smaller than the original input (%d tokens)", got, orig)
	}
}

func TestFinalHistoryPrunedByProviderCompactionInFinalResponse(t *testing.T) {
	model := &mockModel{responses: []*ModelResponse{
		{Items: []RunItem{
			{Type: RunItemCompaction, Compaction: &CompactionData{
				ID: "cmp_1", EncryptedContent: "encrypted-state",
			}},
			{Type: RunItemMessage, Message: &MessageOutput{Text: "final answer"}},
		}},
	}}
	runner := NewRunnerWithModel(model)
	agentDef := &Agent{Name: "test"}
	assistant := &Agent{Name: "assistant"}
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "the user task"}},
		{Type: RunItemMessage, Agent: assistant, Message: &MessageOutput{Text: "old context that was compacted away"}},
		{Type: RunItemMessage, Agent: assistant, Message: &MessageOutput{Text: "more old context"}},
	}

	result, err := runner.Run(context.Background(), agentDef, input, RunConfig{})
	if err != nil {
		t.Fatal(err)
	}
	// FinalHistory must reflect the provider compaction carried by the final
	// response: it starts at the compaction item and drops the pre-compaction
	// assistant transcript. The initial user task is deliberately re-inserted
	// after the blob (PreserveInitialUserMessages parity with local
	// compaction) so it must survive.
	if len(result.FinalHistory) == 0 || result.FinalHistory[0].Type != RunItemCompaction {
		t.Fatalf("expected FinalHistory to start with the provider compaction item, got %+v", result.FinalHistory)
	}
	var taskSurvived bool
	for _, item := range result.FinalHistory {
		if item.Message != nil && strings.Contains(item.Message.Text, "old context") {
			t.Errorf("expected pre-compaction history to be pruned from FinalHistory, found %q", item.Message.Text)
		}
		if item.Message != nil && item.Agent == nil && item.Message.Text == "the user task" {
			taskSurvived = true
		}
	}
	if !taskSurvived {
		t.Errorf("expected the initial user task to survive provider compaction, got %+v", result.FinalHistory)
	}
	last := result.FinalHistory[len(result.FinalHistory)-1]
	if last.Message == nil || last.Message.Text != "final answer" {
		t.Errorf("expected FinalHistory to end with the final answer, got %+v", last)
	}
}
