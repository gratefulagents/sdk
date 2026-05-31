package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHandoffOnHandoffReceivesInput(t *testing.T) {
	expertAgent := &Agent{Name: "expert", Instructions: "Expert agent"}

	model := &mockModel{
		responses: []*ModelResponse{
			{
				Items: []RunItem{
					{Type: RunItemToolCall, ToolCall: &ToolCallData{
						ID: "h1", Name: "transfer_to_expert", Input: json.RawMessage(`{"reason":"billing"}`),
					}},
				},
			},
			{
				Items: []RunItem{
					{Type: RunItemMessage, Message: &MessageOutput{Text: "done"}},
				},
			},
		},
	}

	var gotInput string
	var fired bool
	runner := NewRunnerWithModel(model)
	agent := &Agent{
		Name: "router",
		Handoffs: []*Handoff{NewHandoff(expertAgent, WithOnHandoff(func(_ *RunContext, input json.RawMessage) {
			fired = true
			gotInput = string(input)
		}))},
	}

	if _, err := runner.Run(context.Background(), agent, nil, RunConfig{}); err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("OnHandoff callback was not fired")
	}
	if !strings.Contains(gotInput, "billing") {
		t.Errorf("OnHandoff did not receive model input, got %q", gotInput)
	}
}

func TestHandoffIsEnabledGatesTool(t *testing.T) {
	target := &Agent{Name: "expert"}

	enabled := NewHandoff(target, WithIsEnabled(func(_ *RunContext) bool { return true }))
	disabled := NewHandoff(target, WithIsEnabled(func(_ *RunContext) bool { return false }))
	defaultH := NewHandoff(target)

	if !enabled.ToTool().IsEnabled(nil) {
		t.Error("handoff with IsEnabledFn returning true should be enabled")
	}
	if disabled.ToTool().IsEnabled(nil) {
		t.Error("handoff with IsEnabledFn returning false should be disabled")
	}
	if !defaultH.ToTool().IsEnabled(nil) {
		t.Error("handoff without IsEnabledFn should default to enabled")
	}
}

func TestRemoveAllToolsHandoffInputFilter(t *testing.T) {
	input := []RunItem{
		{Type: RunItemMessage, Message: &MessageOutput{Text: "keep me"}},
		{Type: RunItemToolCall, ToolCall: &ToolCallData{ID: "1", Name: "grep"}},
		{Type: RunItemToolOutput, ToolOutput: &ToolOutputData{CallID: "1", Content: "noise"}},
		{Type: RunItemHandoffCall, HandoffCall: &HandoffCallData{FromAgent: "a", ToAgent: "b"}},
		{Type: RunItemMessage, Message: &MessageOutput{Text: "also keep"}},
	}
	got := RemoveAllToolsHandoffInputFilter(input, nil)
	if len(got) != 2 {
		t.Fatalf("expected 2 items after filtering, got %d", len(got))
	}
	for _, item := range got {
		if item.Type != RunItemMessage {
			t.Errorf("filter left a non-message item: %v", item.Type)
		}
	}
}

func TestWithRecommendedHandoffInstructions(t *testing.T) {
	if got := WithRecommendedHandoffInstructions(""); got != RecommendedHandoffPromptPrefix {
		t.Error("empty instructions should return just the prefix")
	}
	got := WithRecommendedHandoffInstructions("Be helpful.")
	if !strings.HasPrefix(got, RecommendedHandoffPromptPrefix) {
		t.Error("result should start with the recommended prefix")
	}
	if !strings.HasSuffix(got, "Be helpful.") {
		t.Error("result should retain the original instructions")
	}
}

func TestAsToolOutputExtractorAndOverrides(t *testing.T) {
	worker := &Agent{Name: "worker", HandoffDescription: "does work"}
	model := &mockModel{
		responses: []*ModelResponse{
			{Items: []RunItem{{Type: RunItemMessage, Message: &MessageOutput{Text: "raw result"}}}},
		},
	}
	runner := NewRunnerWithModel(model)

	tool := worker.AsTool(runner,
		WithAsToolName("custom_worker"),
		WithAsToolDescription("custom desc"),
		WithAsToolOutputExtractor(func(r *RunResult) string {
			return "extracted:" + r.FinalText()
		}),
	)

	if tool.Name() != "custom_worker" {
		t.Errorf("expected overridden name, got %q", tool.Name())
	}
	if tool.Description() != "custom desc" {
		t.Errorf("expected overridden description, got %q", tool.Description())
	}

	res, err := tool.Execute(context.Background(), json.RawMessage(`{"message":"go"}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "extracted:raw result" {
		t.Errorf("expected extractor-transformed output, got %q", res.Content)
	}
}

func TestAsToolDefaultsUnchanged(t *testing.T) {
	worker := &Agent{Name: "Code Reviewer", HandoffDescription: "reviews"}
	tool := worker.AsTool(NewRunnerWithModel(&mockModel{}))
	if tool.Name() != "agent_Code_Reviewer" {
		t.Errorf("default name changed unexpectedly: %q", tool.Name())
	}
	if tool.Description() != "reviews" {
		t.Errorf("default description changed unexpectedly: %q", tool.Description())
	}
}
