package agent

import "encoding/json"

// InputGuardrail validates input before it reaches the model.
type InputGuardrail struct {
	Name string
	Fn   func(ctx *RunContext, agent *Agent, input []RunItem) (*GuardrailResult, error)
}

// OutputGuardrail validates model output before it's returned.
type OutputGuardrail struct {
	Name string
	Fn   func(ctx *RunContext, agent *Agent, output any) (*GuardrailResult, error)
}

// ToolInputGuardrail validates a tool's input arguments before execution.
type ToolInputGuardrail struct {
	Name string
	Fn   func(ctx *RunContext, agent *Agent, tool Tool, input json.RawMessage) (*GuardrailResult, error)
}

// ToolOutputGuardrail validates a tool's output after execution.
type ToolOutputGuardrail struct {
	Name string
	Fn   func(ctx *RunContext, agent *Agent, tool Tool, result ToolResult) (*GuardrailResult, error)
}

// GuardrailResult is the outcome of a guardrail check.
type GuardrailResult struct {
	Output            any
	TripwireTriggered bool
}

// ToolGuardrailResult is the outcome of a per-tool guardrail check.
type ToolGuardrailResult struct {
	GuardrailName     string
	ToolName          string
	Output            any
	TripwireTriggered bool
}
