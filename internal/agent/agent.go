package agent

import "encoding/json"

// ToolUseBehavior controls what happens after tool calls complete.
type ToolUseBehavior int

const (
	// RunLLMAgain runs the LLM again with the tool results (default).
	RunLLMAgain ToolUseBehavior = iota
	// StopOnFirstTool returns immediately after the first tool result.
	StopOnFirstTool
)

// Agent defines an AI agent with its configuration, tools, and behavior.
type Agent struct {
	Name               string
	Instructions       string                                     // static instructions
	InstructionsFn     func(ctx *RunContext, agent *Agent) string // dynamic instructions (used if set)
	Model              string                                     // model name, resolved to Model impl at runtime
	ModelSettings      ModelSettings
	Tools              []Tool
	MCPServers         []string   // host-provided MCP server names for prompt context
	Handoffs           []*Handoff // in-process agent handoffs
	InputGuardrails    []InputGuardrail
	OutputGuardrails   []OutputGuardrail
	OutputType         *OutputSchema // nil = plain string output
	Hooks              AgentHooks    // per-agent lifecycle hooks
	ToolUseBehavior    ToolUseBehavior
	StopAtTools        *StopAtTools
	ToolsToFinalOutput *ToolsToFinalOutputResult
	HandoffDescription string // description when this agent is a handoff target
}

// GetInstructions resolves the agent's instructions, preferring InstructionsFn if set.
func (a *Agent) GetInstructions(ctx *RunContext) string {
	if a.InstructionsFn != nil {
		return a.InstructionsFn(ctx, a)
	}
	return a.Instructions
}

// GetAllTools returns all tools available to this agent: explicit tools + handoff-generated tools.
func (a *Agent) GetAllTools(ctx *RunContext) []Tool {
	tools := make([]Tool, 0, len(a.Tools)+len(a.Handoffs))
	tools = append(tools, a.Tools...)
	for _, h := range a.Handoffs {
		tools = append(tools, h.ToTool())
	}
	return tools
}

// Clone returns a shallow copy of the agent with optional overrides applied.
func (a *Agent) Clone(opts ...AgentOption) *Agent {
	clone := *a
	for _, opt := range opts {
		opt(&clone)
	}
	return &clone
}

// AgentOption is a functional option for Agent.Clone().
type AgentOption func(*Agent)

// WithName overrides the agent name.
func WithName(name string) AgentOption {
	return func(a *Agent) { a.Name = name }
}

// WithInstructions overrides the agent instructions.
func WithInstructions(instructions string) AgentOption {
	return func(a *Agent) { a.Instructions = instructions }
}

// WithModel overrides the agent model.
func WithModel(model string) AgentOption {
	return func(a *Agent) { a.Model = model }
}

// WithTools overrides the agent tools.
func WithTools(tools ...Tool) AgentOption {
	return func(a *Agent) { a.Tools = tools }
}

// WithHandoffs overrides the agent handoffs.
func WithHandoffs(handoffs ...*Handoff) AgentOption {
	return func(a *Agent) { a.Handoffs = handoffs }
}

// WithOutputType overrides the agent output type.
func WithOutputType(schema *OutputSchema) AgentOption {
	return func(a *Agent) { a.OutputType = schema }
}

// WithStopAtTools configures tool names that stop the run after their results.
func WithStopAtTools(stop *StopAtTools) AgentOption {
	return func(a *Agent) { a.StopAtTools = stop }
}

// WithToolsToFinalOutput configures whether a stopping tool result becomes final output.
func WithToolsToFinalOutput(result *ToolsToFinalOutputResult) AgentOption {
	return func(a *Agent) { a.ToolsToFinalOutput = result }
}

// StopAtTools configures tool names that cause the run to stop when called.
type StopAtTools struct {
	ToolNames []string
}

// Contains checks if the given tool name is in the stop list.
func (s *StopAtTools) Contains(name string) bool {
	for _, n := range s.ToolNames {
		if n == name {
			return true
		}
	}
	return false
}

// ToolsToFinalOutputResult indicates whether tool results should be treated as the final output.
type ToolsToFinalOutputResult struct {
	IsFinalOutput bool
	Output        json.RawMessage
}
