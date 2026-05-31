package agent

import "strings"

// RecommendedHandoffPromptPrefix is a system-prompt preamble that orients an
// agent within a multi-agent system. Prepending it to an agent's instructions
// helps the model treat handoffs as seamless background transfers instead of
// narrating them to the user. It mirrors the guidance shipped by other agent
// frameworks for handoff-aware agents.
const RecommendedHandoffPromptPrefix = `# System context
You are part of a multi-agent system designed to make agent coordination and execution easy. Agents use two primary abstractions: tools and handoffs. Handoffs transfer control to another agent that is better suited for the task, and are achieved by calling a handoff function, generally named ` + "`transfer_to_<agent_name>`" + `. Transfers between agents are handled seamlessly in the background; do not mention or draw attention to these transfers in your conversation with the user.`

// WithRecommendedHandoffInstructions returns the given instructions prefixed
// with RecommendedHandoffPromptPrefix. If instructions is empty, only the prefix
// is returned.
func WithRecommendedHandoffInstructions(instructions string) string {
	if strings.TrimSpace(instructions) == "" {
		return RecommendedHandoffPromptPrefix
	}
	return RecommendedHandoffPromptPrefix + "\n\n" + instructions
}

// RemoveAllToolsHandoffInputFilter is a prebuilt handoff InputFilter that strips
// every tool call and tool output item from the history handed to the receiving
// agent. This keeps the target agent focused on the conversation without
// inheriting potentially large or irrelevant tool-call noise from the prior
// agent. Pass it via WithInputFilter.
func RemoveAllToolsHandoffInputFilter(input []RunItem, _ []RunItem) []RunItem {
	filtered := make([]RunItem, 0, len(input))
	for _, item := range input {
		switch item.Type {
		case RunItemToolCall, RunItemToolOutput, RunItemHandoffCall, RunItemHandoffOutput:
			continue
		default:
			filtered = append(filtered, item)
		}
	}
	return filtered
}
