package agent

import "encoding/json"

// RunResult is the outcome of a Runner.Run() call.
type RunResult struct {
	FinalOutput                any // string or structured (parsed from OutputSchema)
	LastAgent                  *Agent
	NewItems                   []RunItem
	RawResponses               []ModelResponse
	InputGuardrailResults      []InputGuardrailResult
	OutputGuardrailResults     []OutputGuardrailResult
	ToolInputGuardrailResults  []ToolGuardrailResult
	ToolOutputGuardrailResults []ToolGuardrailResult
	Usage                      Usage
	Interruption               *Interruption   // non-nil if run was interrupted (e.g. approval gate); first of Interruptions
	Interruptions              []*Interruption // all pending interruptions from the turn (parallel tool calls can trigger several)
	LastResponseID             string          // last model response ID for continuation
}

// ToInputList converts the result's items into a format suitable for resuming a run.
func (r *RunResult) ToInputList() []RunItem {
	return r.NewItems
}

// FinalText returns the final output as a string, or empty if not a string output.
func (r *RunResult) FinalText() string {
	if s, ok := r.FinalOutput.(string); ok {
		return s
	}
	return ""
}

// IsInterrupted returns true if the run was interrupted.
func (r *RunResult) IsInterrupted() bool {
	return r.Interruption != nil
}

// AllInterruptions returns every pending interruption from the run. It falls
// back to the singular Interruption field for results built before the
// Interruptions slice existed.
func (r *RunResult) AllInterruptions() []*Interruption {
	if len(r.Interruptions) > 0 {
		return r.Interruptions
	}
	if r.Interruption != nil {
		return []*Interruption{r.Interruption}
	}
	return nil
}

// ToState converts the result into a RunState for resuming an interrupted run.
func (r *RunResult) ToState() *RunState {
	return &RunState{
		Items:          r.NewItems,
		LastAgent:      r.LastAgent,
		Interruption:   r.Interruption,
		Interruptions:  r.Interruptions,
		LastResponseID: r.LastResponseID,
	}
}

// RunState captures enough state to resume an interrupted run.
type RunState struct {
	Items          []RunItem
	LastAgent      *Agent
	Interruption   *Interruption
	Interruptions  []*Interruption
	LastResponseID string
}

// Interruption records why a run was paused (e.g. tool needs approval).
type Interruption struct {
	ToolName   string          `json:"tool_name"`
	ToolInput  json.RawMessage `json:"tool_input"`
	ToolCallID string          `json:"tool_call_id"`
}

// InputGuardrailResult is the outcome of an input guardrail check.
type InputGuardrailResult struct {
	GuardrailName     string
	Output            any
	TripwireTriggered bool
}

// OutputGuardrailResult is the outcome of an output guardrail check.
type OutputGuardrailResult struct {
	GuardrailName     string
	Output            any
	TripwireTriggered bool
}
