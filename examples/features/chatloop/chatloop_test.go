package chatloop_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/examples/features/internal/liverunner"
	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

type memorySession struct {
	messages []agentsdk.UserMessage
	items    []agentsdk.RunItem
}

func (s *memorySession) LoadMessages(context.Context, agentsdk.Cursor, int) ([]agentsdk.UserMessage, agentsdk.Cursor, error) {
	return append([]agentsdk.UserMessage(nil), s.messages...), agentsdk.Cursor{}, nil
}

func (s *memorySession) AppendRunItems(_ context.Context, items []agentsdk.RunItem) error {
	s.items = append(s.items, items...)
	return nil
}

func (s *memorySession) WorkingState(context.Context) (agentsdk.WorkingState, error) {
	return agentsdk.WorkingState{CurrentStep: "demo"}, nil
}

func TestChatLoopExample(t *testing.T) {
	runner, model := liverunner.Runner(t)
	session := &memorySession{messages: []agentsdk.UserMessage{{ID: 1, Content: "Reply with exactly the single word PINEAPPLE in upper case."}}}

	result, err := agentsdk.NewChatLoop(agentsdk.ChatLoopOptions{
		Runner:       runner,
		SessionStore: session,
		Agent: &agentsdk.Agent{
			Name:         "chatloop-demo",
			Model:        model,
			Instructions: "Follow the user's exact instruction.",
		},
		RunConfig: agentsdk.RunConfig{MaxTurns: 2},
	}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(result.FinalText()), "PINEAPPLE") {
		t.Fatalf("FinalText() = %q, want sentinel PINEAPPLE", result.FinalText())
	}
	var sawAssistantMessage bool
	for _, item := range session.items {
		if item.Type == agentsdk.RunItemMessage && item.Message != nil &&
			strings.Contains(strings.ToUpper(item.Message.Text), "PINEAPPLE") {
			sawAssistantMessage = true
			break
		}
	}
	if !sawAssistantMessage {
		t.Fatalf("expected loop to persist an assistant message containing PINEAPPLE in session.items: %#v", session.items)
	}
}
