package agentsdk

import (
	"encoding/json"
	"strings"
	"testing"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

func TestIsSessionModeSlashCommand(t *testing.T) {
	tests := []struct {
		message string
		want    bool
	}{
		{message: "/plan", want: true},
		{message: " /chat ", want: true},
		{message: "/mode ultrawork", want: true},
		{message: "/exit-plan", want: false},
		{message: "/status", want: false},
		{message: "implement this", want: false},
	}
	for _, tt := range tests {
		if got := IsSessionModeSlashCommand(tt.message); got != tt.want {
			t.Fatalf("IsSessionModeSlashCommand(%q) = %v, want %v", tt.message, got, tt.want)
		}
	}
}

func TestBuildPhaseApprovalChangeRequest(t *testing.T) {
	systemMessage, prompt := BuildPhaseApprovalChangeRequest("reporting", "We still need to fix the realtime bugs.")
	if !strings.Contains(systemMessage, "did not approve phase 'reporting'") {
		t.Fatalf("systemMessage = %q, want reporting denial context", systemMessage)
	}
	if prompt != "We still need to fix the realtime bugs." {
		t.Fatalf("prompt = %q, want original feedback", prompt)
	}

	_, genericPrompt := BuildPhaseApprovalChangeRequest("reporting", "request changes")
	if genericPrompt != "Please rework phase 'reporting' and continue. It has not been approved yet." {
		t.Fatalf("generic prompt = %q", genericPrompt)
	}
}

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

func TestPausePromptHelpers(t *testing.T) {
	question, actions := BuildPhaseApprovalPrompt("shipping")
	if !strings.Contains(question, "Phase 'shipping' requires your approval") {
		t.Fatalf("phase approval question = %q", question)
	}
	if !strings.Contains(string(actions), `"id":"approve"`) {
		t.Fatalf("phase approval actions = %s", string(actions))
	}

	question, actions = BuildPhaseTurnLimitPrompt("implementation", 4)
	if !strings.Contains(question, "4 turns") {
		t.Fatalf("phase turn limit question = %q", question)
	}
	if !strings.Contains(string(actions), `"id":"change_approach"`) {
		t.Fatalf("phase turn limit actions = %s", string(actions))
	}

	question, actions = BuildModeResetApprovalPrompt("source", "target")
	if question != "Mode 'source' completed. Approve reset to 'target'?" {
		t.Fatalf("mode reset question = %q", question)
	}
	if !strings.Contains(string(actions), `"style":"destructive"`) {
		t.Fatalf("mode reset actions = %s", string(actions))
	}

	if got := BuildAutoTurnCapPrompt(123); got != "Auto mode global turn cap (123) reached." {
		t.Fatalf("BuildAutoTurnCapPrompt() = %q", got)
	}
}

func TestEvaluatePhaseTurnLimit(t *testing.T) {
	decision := EvaluatePhaseTurnLimit("implementation", 2, 3)
	if decision.Exceeded {
		t.Fatalf("Exceeded = true, want false")
	}
	if decision.NextCount != 3 {
		t.Fatalf("NextCount = %d, want 3", decision.NextCount)
	}

	decision = EvaluatePhaseTurnLimit("implementation", 3, 3)
	if !decision.Exceeded {
		t.Fatalf("Exceeded = false, want true")
	}
	if decision.NextCount != 4 {
		t.Fatalf("NextCount = %d, want 4", decision.NextCount)
	}
	if !strings.Contains(decision.Question, "turn limit (3 turns)") {
		t.Fatalf("Question = %q", decision.Question)
	}
	if !strings.Contains(string(decision.Actions), `"id":"continue"`) {
		t.Fatalf("Actions = %s", string(decision.Actions))
	}

	decision = EvaluatePhaseTurnLimit("implementation", 9, 0)
	if decision.Exceeded || decision.NextCount != 9 {
		t.Fatalf("unlimited decision = %#v, want no-op", decision)
	}
}

func TestIsApprovalReply(t *testing.T) {
	if !IsApprovalReply(" approve ") {
		t.Fatalf("IsApprovalReply() = false, want true")
	}
	if IsApprovalReply("approve with changes") {
		t.Fatalf("IsApprovalReply() = true, want false")
	}
}

func TestBuildModeResetChangeRequest(t *testing.T) {
	systemMessage, prompt := BuildModeResetChangeRequest("source-mode", "target-mode", "Deny: please add regression coverage before you reset")
	if !strings.Contains(systemMessage, "did not approve resetting mode 'source-mode' to 'target-mode'") {
		t.Fatalf("systemMessage = %q, want reset denial context", systemMessage)
	}
	if prompt != "please add regression coverage before you reset" {
		t.Fatalf("prompt = %q, want stripped user feedback", prompt)
	}

	_, genericPrompt := BuildModeResetChangeRequest("source-mode", "target-mode", "deny")
	if genericPrompt != "Do not reset to 'target-mode' yet. Continue working in mode 'source-mode' and resume from the appropriate phase (shipping or earlier)." {
		t.Fatalf("generic prompt = %q", genericPrompt)
	}
}

func TestEvaluateModeCompletion(t *testing.T) {
	if got := EvaluateModeCompletion("source", false, &sdkmode.ResetTo{Name: "target"}); got.Completed {
		t.Fatalf("Completed = true, want false")
	}

	got := EvaluateModeCompletion("source", true, nil)
	if !got.Completed || got.ShouldReset || got.RequiresApproval {
		t.Fatalf("nil reset decision = %#v", got)
	}

	got = EvaluateModeCompletion("source", true, &sdkmode.ResetTo{Name: "target", Prompt: "hello", ClearHistory: true})
	if !got.Completed || !got.ShouldReset || got.RequiresApproval {
		t.Fatalf("reset decision = %#v", got)
	}
	if got.TargetMode != "target" || got.SeedPrompt != "hello" || !got.ClearHistory {
		t.Fatalf("reset fields = %#v", got)
	}

	got = EvaluateModeCompletion("source", true, &sdkmode.ResetTo{Name: "target", RequiresApproval: true})
	if !got.Completed || got.ShouldReset || !got.RequiresApproval {
		t.Fatalf("approval decision = %#v", got)
	}
	if !strings.Contains(got.Question, "Approve reset to 'target'") {
		t.Fatalf("Question = %q", got.Question)
	}
	if !strings.Contains(string(got.Actions), `"id":"deny"`) {
		t.Fatalf("Actions = %s", string(got.Actions))
	}
}

func TestBuildPhaseTrackingDirective(t *testing.T) {
	directive := BuildPhaseTrackingDirective(&sdkmode.TemplateSpec{
		Phases: []sdkmode.Phase{
			{ID: "explore", ReadOnly: true},
			{ID: "implement"},
			{ID: "ship"},
		},
	})
	if !strings.Contains(directive, "This mode defines phases: explore -> implement -> ship") {
		t.Fatalf("directive missing phase order:\n%s", directive)
	}
	if !strings.Contains(directive, "Call set_phase(\"explore\")") {
		t.Fatalf("directive missing first-phase instruction:\n%s", directive)
	}
	if !strings.Contains(directive, "Read-only phases (explore)") {
		t.Fatalf("directive missing read-only note:\n%s", directive)
	}

	if got := BuildPhaseTrackingDirective(&sdkmode.TemplateSpec{}); got != "" {
		t.Fatalf("empty directive = %q, want empty", got)
	}
}

func TestResolvePhaseToolAccess(t *testing.T) {
	spec := &sdkmode.TemplateSpec{Phases: []sdkmode.Phase{
		{ID: "research", ReadOnly: true},
		{ID: "ship"},
	}}

	access, downgraded := ResolvePhaseToolAccess(ToolAccessLevelFull, spec, "research")
	if access != ToolAccessLevelReadOnly || !downgraded {
		t.Fatalf("read-only phase access = %q, %v", access, downgraded)
	}

	access, downgraded = ResolvePhaseToolAccess(ToolAccessLevelFull, spec, "ship")
	if access != ToolAccessLevelFull || downgraded {
		t.Fatalf("writable phase access = %q, %v", access, downgraded)
	}

	access, downgraded = ResolvePhaseToolAccess(ToolAccessLevelReadOnly, nil, "research")
	if access != ToolAccessLevelReadOnly || downgraded {
		t.Fatalf("nil snapshot access = %q, %v", access, downgraded)
	}
}
