package agent

import (
	"fmt"
	"time"
)

// AgentError is the base error type for agent operations.
type AgentError struct {
	Message string
	Cause   error
}

func (e *AgentError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *AgentError) Unwrap() error { return e.Cause }

// MaxTurnsExceeded is returned when Runner.Run exceeds the configured max turns.
type MaxTurnsExceeded struct {
	MaxTurns int
}

func (e *MaxTurnsExceeded) Error() string {
	return fmt.Sprintf("max turns exceeded: %d", e.MaxTurns)
}

// ModelBehaviorError indicates the model produced invalid or unexpected output.
type ModelBehaviorError struct {
	Message string
	Model   string
}

func (e *ModelBehaviorError) Error() string {
	return fmt.Sprintf("model behavior error (%s): %s", e.Model, e.Message)
}

// UserError indicates invalid user input.
type UserError struct {
	Message string
}

func (e *UserError) Error() string { return e.Message }

// ToolTimeoutError is returned when a tool execution exceeds its timeout.
type ToolTimeoutError struct {
	ToolName string
	Timeout  time.Duration
}

func (e *ToolTimeoutError) Error() string {
	return fmt.Sprintf("tool %q timed out after %s", e.ToolName, e.Timeout)
}

// InputGuardrailTripwireTriggered is returned when an input guardrail's tripwire fires.
type InputGuardrailTripwireTriggered struct {
	GuardrailName string
	Result        GuardrailResult
}

func (e *InputGuardrailTripwireTriggered) Error() string {
	return fmt.Sprintf("input guardrail %q tripwire triggered", e.GuardrailName)
}

// OutputGuardrailTripwireTriggered is returned when an output guardrail's tripwire fires.
type OutputGuardrailTripwireTriggered struct {
	GuardrailName string
	Result        GuardrailResult
}

func (e *OutputGuardrailTripwireTriggered) Error() string {
	return fmt.Sprintf("output guardrail %q tripwire triggered", e.GuardrailName)
}

// ToolInputGuardrailTripwireTriggered is returned when a tool input guardrail's tripwire fires.
type ToolInputGuardrailTripwireTriggered struct {
	GuardrailName string
	ToolName      string
	Result        GuardrailResult
}

func (e *ToolInputGuardrailTripwireTriggered) Error() string {
	return fmt.Sprintf("tool input guardrail %q tripwire triggered for tool %q", e.GuardrailName, e.ToolName)
}

// ToolOutputGuardrailTripwireTriggered is returned when a tool output guardrail's tripwire fires.
type ToolOutputGuardrailTripwireTriggered struct {
	GuardrailName string
	ToolName      string
	Result        GuardrailResult
}

func (e *ToolOutputGuardrailTripwireTriggered) Error() string {
	return fmt.Sprintf("tool output guardrail %q tripwire triggered for tool %q", e.GuardrailName, e.ToolName)
}

// RunErrorDetails provides structured context about a run-level error.
type RunErrorDetails struct {
	Error error
	Agent *Agent
	Turn  int
	Items []RunItem
}

func (e *RunErrorDetails) Unwrap() error { return e.Error }
