package agentsdk

import (
	"encoding/json"
	"fmt"
	"strings"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

type QuickAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Mode  string `json:"mode,omitempty"`
	Style string `json:"style,omitempty"`
}

type PhaseTurnLimitDecision struct {
	NextCount int32
	Exceeded  bool
	Question  string
	Actions   json.RawMessage
}

type ModeCompletionDecision struct {
	Completed        bool
	ShouldReset      bool
	RequiresApproval bool
	TargetMode       string
	SeedPrompt       string
	ClearHistory     bool
	Question         string
	Actions          json.RawMessage
}

type UserInputPause struct {
	Requested bool
	Question  string
	Actions   json.RawMessage
}

func MarshalQuickActions(actions ...QuickAction) json.RawMessage {
	data, _ := json.Marshal(actions)
	return data
}

func ExtractAskUserChoices(input json.RawMessage) json.RawMessage {
	var in struct {
		Choices []string `json:"choices"`
	}
	if err := json.Unmarshal(input, &in); err != nil || len(in.Choices) == 0 {
		return nil
	}

	actions := make([]QuickAction, len(in.Choices))
	for i, choice := range in.Choices {
		style := "secondary"
		if i == 0 {
			style = "primary"
		}
		actions[i] = QuickAction{
			ID:    fmt.Sprintf("choice_%d", i),
			Label: choice,
			Style: style,
		}
	}
	return MarshalQuickActions(actions...)
}

func ExtractPresentPlanData(input json.RawMessage) (string, json.RawMessage) {
	var in struct {
		Summary     string          `json:"summary"`
		Actions     json.RawMessage `json:"actions"`
		Recommended string          `json:"recommended"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "", nil
	}
	return in.Summary, in.Actions
}

func DetectUserInputPause(items []RunItem, finalText string) UserInputPause {
	for _, item := range items {
		if item.Type != RunItemToolCall || item.ToolCall == nil {
			continue
		}
		switch item.ToolCall.Name {
		case "AskUserQuestion":
			question := ExtractAskUserQuestion(item.ToolCall.Input)
			if question == "" || (looksLikeRawJSONObject(question) && strings.TrimSpace(finalText) != "") {
				question = strings.TrimSpace(finalText)
			}
			if question == "" {
				question = "The agent needs your input to continue."
			}
			return UserInputPause{
				Requested: true,
				Question:  question,
				Actions:   ExtractAskUserChoices(item.ToolCall.Input),
			}
		case "present_plan":
			question, actions := ExtractPresentPlanData(item.ToolCall.Input)
			if question == "" {
				question = strings.TrimSpace(finalText)
			}
			if question == "" {
				question = "The agent needs your input to continue."
			}
			return UserInputPause{
				Requested: true,
				Question:  question,
				Actions:   actions,
			}
		}
	}
	return UserInputPause{}
}

func looksLikeRawJSONObject(text string) bool {
	trimmed := strings.TrimSpace(text)
	return strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")
}

func BuildPhaseApprovalPrompt(pendingPhase string) (question string, actions json.RawMessage) {
	question = fmt.Sprintf("Phase '%s' requires your approval. Review the plan using the Plan button above. If you want changes, write them below.", pendingPhase)
	actions = MarshalQuickActions(QuickAction{ID: "approve", Label: "Approve", Style: "primary"})
	return question, actions
}

func BuildPhaseTurnLimitPrompt(phase string, maxTurns int) (question string, actions json.RawMessage) {
	question = fmt.Sprintf("Phase '%s' has reached its turn limit (%d turns). Continue or change approach?", phase, maxTurns)
	actions = MarshalQuickActions(
		QuickAction{ID: "continue", Label: "Continue"},
		QuickAction{ID: "change_approach", Label: "Change Approach"},
	)
	return question, actions
}

func EvaluatePhaseTurnLimit(phase string, currentCount int32, maxTurns int) PhaseTurnLimitDecision {
	if maxTurns <= 0 {
		return PhaseTurnLimitDecision{NextCount: currentCount}
	}
	nextCount := currentCount + 1
	if int(nextCount) <= maxTurns {
		return PhaseTurnLimitDecision{NextCount: nextCount}
	}
	question, actions := BuildPhaseTurnLimitPrompt(phase, maxTurns)
	return PhaseTurnLimitDecision{
		NextCount: nextCount,
		Exceeded:  true,
		Question:  question,
		Actions:   actions,
	}
}

func BuildModeResetApprovalPrompt(currentMode, targetMode string) (question string, actions json.RawMessage) {
	question = fmt.Sprintf("Mode '%s' completed. Approve reset to '%s'?", currentMode, targetMode)
	actions = MarshalQuickActions(
		QuickAction{ID: "approve", Label: "Approve", Style: "primary"},
		QuickAction{ID: "deny", Label: "Deny", Style: "destructive"},
	)
	return question, actions
}

func EvaluateModeCompletion(currentMode string, completionRequested bool, resetTo *sdkmode.ResetTo) ModeCompletionDecision {
	if !completionRequested {
		return ModeCompletionDecision{}
	}
	if resetTo == nil || strings.TrimSpace(resetTo.Name) == "" {
		return ModeCompletionDecision{Completed: true}
	}

	targetMode := strings.TrimSpace(resetTo.Name)
	decision := ModeCompletionDecision{
		Completed:    true,
		ShouldReset:  true,
		TargetMode:   targetMode,
		SeedPrompt:   resetTo.Prompt,
		ClearHistory: resetTo.ClearHistory,
	}
	if resetTo.RequiresApproval {
		decision.RequiresApproval = true
		decision.ShouldReset = false
		decision.Question, decision.Actions = BuildModeResetApprovalPrompt(currentMode, targetMode)
	}
	return decision
}

func BuildPhaseTrackingDirective(spec *sdkmode.TemplateSpec) string {
	if spec == nil || len(spec.Phases) == 0 {
		return ""
	}

	phases := make([]string, 0, len(spec.Phases))
	var readOnlyPhases []string
	for _, phase := range spec.Phases {
		id := strings.TrimSpace(phase.ID)
		if id == "" {
			continue
		}
		phases = append(phases, id)
		if phase.ReadOnly {
			readOnlyPhases = append(readOnlyPhases, id)
		}
	}
	if len(phases) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Phase Tracking (MANDATORY)\n\n")
	b.WriteString("This mode defines phases: ")
	b.WriteString(strings.Join(phases, " -> "))
	b.WriteString("\n\n")
	b.WriteString("You MUST call the set_phase tool to track your progress:\n")
	b.WriteString("1. Call set_phase(\"" + phases[0] + "\") as your VERY FIRST action before doing anything else.\n")
	b.WriteString("2. Call set_phase at each transition - do not skip phases or work outside a phase.\n")
	b.WriteString("3. The phase timeline is displayed to the user. If you do not call set_phase, it looks broken.\n")

	if len(readOnlyPhases) > 0 {
		b.WriteString("4. Read-only phases (" + strings.Join(readOnlyPhases, ", ") + "): you can read/explore but cannot edit files or run mutating commands.\n")
	}

	return b.String()
}

func ResolvePhaseToolAccess(defaultAccess ToolAccessLevel, spec *sdkmode.TemplateSpec, phaseID string) (ToolAccessLevel, bool) {
	phaseID = strings.TrimSpace(phaseID)
	if spec == nil || phaseID == "" {
		return defaultAccess, false
	}
	for _, phase := range spec.Phases {
		if phase.ID == phaseID && phase.ReadOnly {
			return ToolAccessLevelReadOnly, true
		}
	}
	return defaultAccess, false
}

func BuildAutoTurnCapPrompt(maxTurns int) string {
	return fmt.Sprintf("Auto mode global turn cap (%d) reached.", maxTurns)
}

func IsApprovalReply(reply string) bool {
	return strings.EqualFold(strings.TrimSpace(reply), "approve")
}

func BuildPhaseApprovalChangeRequest(pendingPhase, reply string) (systemMessage, prompt string) {
	trimmed := NormalizeApprovalFeedback(reply, "request changes", "request_changes")
	systemMessage = fmt.Sprintf(
		"[SYSTEM] The user did not approve phase '%s'. Do not treat that phase as approved. Rework the plan or implementation, continue in an earlier or non-gated phase as needed, and request approval again only when ready.",
		pendingPhase,
	)

	switch {
	case trimmed == "":
		prompt = fmt.Sprintf("Please rework phase '%s' and continue. It has not been approved yet.", pendingPhase)
	default:
		prompt = trimmed
	}

	return systemMessage, prompt
}

func BuildModeResetChangeRequest(currentMode, targetMode, reply string) (systemMessage, prompt string) {
	trimmed := NormalizeApprovalFeedback(reply, "deny", "request changes", "request_changes")
	systemMessage = fmt.Sprintf(
		"[SYSTEM] The user did not approve resetting mode '%s' to '%s'. Treat the prior completion as provisional, stay in mode '%s', resume from the appropriate phase (shipping or earlier) to address the feedback, update the existing branch or PR if one already exists, and request reset again only when the work is truly complete.",
		currentMode,
		targetMode,
		currentMode,
	)

	if trimmed == "" {
		prompt = fmt.Sprintf(
			"Do not reset to '%s' yet. Continue working in mode '%s' and resume from the appropriate phase (shipping or earlier).",
			targetMode,
			currentMode,
		)
		return systemMessage, prompt
	}

	return systemMessage, trimmed
}

func NormalizeApprovalFeedback(reply string, actionPrefixes ...string) string {
	trimmed := strings.TrimSpace(reply)
	if trimmed == "" {
		return ""
	}

	lower := strings.ToLower(trimmed)
	for _, prefix := range actionPrefixes {
		prefixLower := strings.ToLower(strings.TrimSpace(prefix))
		if prefixLower == "" {
			continue
		}
		switch {
		case lower == prefixLower:
			return ""
		case strings.HasPrefix(lower, prefixLower+":"):
			return strings.TrimSpace(trimmed[len(prefixLower)+1:])
		case strings.HasPrefix(lower, prefixLower+" "):
			return strings.TrimSpace(trimmed[len(prefixLower):])
		}
	}

	return trimmed
}
