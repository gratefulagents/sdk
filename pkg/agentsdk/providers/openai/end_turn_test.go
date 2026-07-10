package openai

import (
	"testing"

	internalanthropic "github.com/gratefulagents/sdk/internal/anthropic"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestConvertAnthropicResponsePreservesEndTurnAndMessagePhase(t *testing.T) {
	keepGoing := false
	block := internalanthropic.NewTextBlock("continuing")
	block.Phase = "commentary"

	got := convertAnthropicResponse(&internalanthropic.CreateMessageResponse{
		Content: []internalanthropic.ContentBlock{block},
		EndTurn: &keepGoing,
	})

	if got.EndTurn == nil || *got.EndTurn {
		t.Fatalf("EndTurn = %v, want explicit false", got.EndTurn)
	}
	if len(got.Items) != 1 || got.Items[0].Message == nil {
		t.Fatalf("items = %+v, want one message", got.Items)
	}
	if got.Items[0].Message.Phase != "commentary" {
		t.Fatalf("message phase = %q, want commentary", got.Items[0].Message.Phase)
	}
}

func TestItemsToAnthropicMessagesPreservesAssistantMessagePhase(t *testing.T) {
	messages := itemsToAnthropicMessages([]agentsdk.RunItem{{
		Type:  agentsdk.RunItemMessage,
		Agent: &agentsdk.Agent{Name: "assistant"},
		Message: &agentsdk.MessageOutput{
			Text:  "continuing",
			Phase: "commentary",
		},
	}})

	if len(messages) != 1 || len(messages[0].Content) != 1 {
		t.Fatalf("messages = %+v, want one content block", messages)
	}
	if got := messages[0].Content[0].Phase; got != "commentary" {
		t.Fatalf("phase = %q, want commentary", got)
	}
}
