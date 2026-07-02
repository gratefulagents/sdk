package agentsdk

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestQuickActionHelpers(t *testing.T) {
	data := MarshalQuickActions(
		QuickAction{ID: "approve", Label: "Approve", Style: "primary"},
		QuickAction{ID: "request_changes", Label: "Request Changes"},
	)

	var actions []map[string]string
	if err := json.Unmarshal(data, &actions); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(actions))
	}
	if got := actions[0]["id"]; got != "approve" {
		t.Fatalf("actions[0][\"id\"] = %q, want approve", got)
	}
	if got := actions[1]["id"]; got != "request_changes" {
		t.Fatalf("actions[1][\"id\"] = %q, want request_changes", got)
	}
	if _, ok := actions[0]["value"]; ok {
		t.Fatalf("actions[0] unexpectedly serialized legacy \"value\" key: %#v", actions[0])
	}
}

func TestExtractAskUserChoices(t *testing.T) {
	data := ExtractAskUserChoices(json.RawMessage(`{"choices":["Ship it","Revise"]}`))
	if len(data) == 0 {
		t.Fatalf("ExtractAskUserChoices() returned no actions")
	}

	var actions []QuickAction
	if err := json.Unmarshal(data, &actions); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("len(actions) = %d, want 2", len(actions))
	}
	if actions[0].ID != "choice_0" || actions[0].Label != "Ship it" || actions[0].Style != "primary" {
		t.Fatalf("actions[0] = %#v", actions[0])
	}
	if actions[1].Style != "secondary" {
		t.Fatalf("actions[1].Style = %q, want secondary", actions[1].Style)
	}

	if got := ExtractAskUserChoices(json.RawMessage(`{"question":"Continue?"}`)); got != nil {
		t.Fatalf("ExtractAskUserChoices() = %s, want nil", string(got))
	}
}

func TestExtractPresentPlanData(t *testing.T) {
	summary, actions := ExtractPresentPlanData(json.RawMessage(`{"summary":"Do the thing","actions":[{"id":"approve","label":"Approve"}],"recommended":"approve"}`))
	if summary != "Do the thing" {
		t.Fatalf("summary = %q", summary)
	}
	var parsed []QuickAction
	if err := json.Unmarshal(actions, &parsed); err != nil {
		t.Fatalf("json.Unmarshal(actions) error = %v", err)
	}
	if len(parsed) != 1 || parsed[0].ID != "approve" {
		t.Fatalf("actions = %#v", parsed)
	}
}

func TestDetectUserInputPause(t *testing.T) {
	pause := DetectUserInputPause([]RunItem{{
		Type: RunItemToolCall,
		ToolCall: &ToolCallData{
			Name:  "AskUserQuestion",
			Input: json.RawMessage(`{"question":"Continue?","choices":["Yes","No"]}`),
		},
	}}, "")
	if !pause.Requested || pause.Question != "Continue?" {
		t.Fatalf("pause = %#v", pause)
	}
	if !strings.Contains(string(pause.Actions), `"label":"Yes"`) {
		t.Fatalf("actions = %s", string(pause.Actions))
	}

	pause = DetectUserInputPause([]RunItem{{
		Type: RunItemToolCall,
		ToolCall: &ToolCallData{
			Name:  "present_plan",
			Input: json.RawMessage(`{"summary":"Review this plan","actions":[{"id":"approve","label":"Approve"}]}`),
		},
	}}, "")
	if !pause.Requested || pause.Question != "Review this plan" {
		t.Fatalf("plan pause = %#v", pause)
	}

	pause = DetectUserInputPause([]RunItem{{
		Type: RunItemToolCall,
		ToolCall: &ToolCallData{
			Name:  "AskUserQuestion",
			Input: json.RawMessage(`{}`),
		},
	}}, "fallback text")
	if pause.Question != "fallback text" {
		t.Fatalf("fallback question = %q", pause.Question)
	}
}

func TestBuildAutoTurnCapPrompt(t *testing.T) {
	if got := BuildAutoTurnCapPrompt(123); got != "Auto mode global turn cap (123) reached." {
		t.Fatalf("BuildAutoTurnCapPrompt() = %q", got)
	}
}
