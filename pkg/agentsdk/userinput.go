package agentsdk

import (
	"encoding/json"
	"fmt"
	"strings"
)

type QuickAction struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Mode  string `json:"mode,omitempty"`
	Style string `json:"style,omitempty"`
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

func BuildAutoTurnCapPrompt(maxTurns int) string {
	return fmt.Sprintf("Auto mode global turn cap (%d) reached.", maxTurns)
}
