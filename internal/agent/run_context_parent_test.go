package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestParentRunItemsContextIsDeepCopied(t *testing.T) {
	original := []RunItem{
		{
			Type: RunItemMessage,
			Message: &MessageOutput{
				Text:   "original",
				Images: []ImageAttachment{{MediaType: "image/png", Data: "pixels"}},
			},
		},
		{
			Type:     RunItemToolCall,
			ToolCall: &ToolCallData{ID: "call", Name: "tool", Input: json.RawMessage(`{"value":1}`)},
		},
		{
			Type:         RunItemToolApproval,
			ToolApproval: &ToolApprovalData{ToolName: "tool", Input: json.RawMessage(`{"approved":true}`)},
		},
	}

	ctx := WithParentRunItems(context.Background(), original)
	original[0].Message.Text = "mutated"
	original[0].Message.Images[0].Data = "mutated"
	original[1].ToolCall.Input[0] = '['
	original[2].ToolApproval.Input[0] = '['

	first := ParentRunItemsFromContext(ctx)
	if first[0].Message.Text != "original" || first[0].Message.Images[0].Data != "pixels" {
		t.Fatalf("captured message was mutated: %#v", first[0].Message)
	}
	if string(first[1].ToolCall.Input) != `{"value":1}` || string(first[2].ToolApproval.Input) != `{"approved":true}` {
		t.Fatalf("captured raw input was mutated: %s / %s", first[1].ToolCall.Input, first[2].ToolApproval.Input)
	}

	first[0].Message.Text = "changed returned copy"
	first[1].ToolCall.Input[0] = '['
	second := ParentRunItemsFromContext(ctx)
	if second[0].Message.Text != "original" || string(second[1].ToolCall.Input) != `{"value":1}` {
		t.Fatalf("returned context was not detached: %#v", second)
	}
}

func TestSchedulerCheckpointRoundTripPreservesParentContextProvenance(t *testing.T) {
	checkpoint := SubAgentSchedulerCheckpoint{Records: []SubAgentSchedulerCheckpointRecord{{
		Task: SubAgentTask{ID: "task_context", AgentName: "worker", Status: SubAgentTaskCompleted},
		ParentContext: []LLMRunItemSnapshot{
			{Type: "message", MessageText: "user"},
			{Type: "message", AgentName: "unregistered-parent", MessageText: "assistant"},
		},
	}}}
	registry := NewSubAgentRegistry(SubAgentRegistryConfig{})
	if err := registry.RestoreSchedulerCheckpoint(checkpoint); err != nil {
		t.Fatal(err)
	}
	roundTrip := registry.SchedulerCheckpoint()
	if len(roundTrip.Records) != 1 || len(roundTrip.Records[0].ParentContext) != 2 {
		t.Fatalf("round-trip checkpoint = %#v", roundTrip)
	}
	if got := roundTrip.Records[0].ParentContext[1].AgentName; got != "unregistered-parent" {
		t.Fatalf("assistant provenance = %q", got)
	}
}
