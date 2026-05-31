package mcp

import (
	"strings"
	"testing"
)

func TestBreakGlassQuestionAndActions(t *testing.T) {
	question := BuildBreakGlassQuestion(BreakGlassRequest{
		Server: "github",
		Tool:   "create_issue",
		Reason: "Need to file the tracked issue",
	}, BreakGlassPolicy{RequireAuditReason: true})
	if !strings.Contains(question, `server "github" tool "create_issue"`) {
		t.Fatalf("question = %q", question)
	}
	if !strings.Contains(question, "Need to file the tracked issue") {
		t.Fatalf("question missing reason: %q", question)
	}
	if !strings.Contains(question, "audit reason") {
		t.Fatalf("question missing audit hint: %q", question)
	}

	actions := string(BreakGlassActions())
	if !strings.Contains(actions, `"id":"approve"`) || !strings.Contains(actions, `"id":"reject"`) {
		t.Fatalf("actions = %s", actions)
	}
}

func TestBlockedMessages(t *testing.T) {
	toolMessage := BlockedToolMessage("github", "create_issue", true)
	if !strings.Contains(toolMessage, "RequestMCPBreakGlass") {
		t.Fatalf("toolMessage = %q", toolMessage)
	}
	serverMessage := BlockedServerMessage("github", false)
	if strings.Contains(serverMessage, "RequestMCPBreakGlass") {
		t.Fatalf("serverMessage = %q", serverMessage)
	}
}
