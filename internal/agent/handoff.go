package agent

import (
	"context"
	"encoding/json"
	"fmt"
)

// Handoff defines an agent-to-agent handoff.
type Handoff struct {
	Agent       *Agent
	ToolName    string // defaults to "transfer_to_{name}"
	Description string
	InputFilter func(input []RunItem, history []RunItem) []RunItem
	InputType   *OutputSchema

	// OnHandoff is an optional callback fired when the model triggers this
	// handoff. It receives the raw JSON arguments the model supplied for the
	// handoff tool call (validated against InputType when InputType is set).
	// Use it to record escalation data, seed shared state, or run side effects
	// before control transfers to the target agent.
	OnHandoff func(ctx *RunContext, input json.RawMessage)

	// IsEnabledFn, when set, gates whether this handoff is exposed to the model
	// for the current run context. Returning false hides the handoff tool, so
	// hosts can enable/disable handoffs per phase or per state without rebuilding
	// the agent. A nil IsEnabledFn means the handoff is always enabled.
	IsEnabledFn func(ctx *RunContext) bool
}

// HandoffOption is a functional option for NewHandoff.
type HandoffOption func(*Handoff)

// NewHandoff creates a Handoff to the target agent.
func NewHandoff(target *Agent, opts ...HandoffOption) *Handoff {
	h := &Handoff{
		Agent:    target,
		ToolName: fmt.Sprintf("transfer_to_%s", target.Name),
	}
	if target.HandoffDescription != "" {
		h.Description = target.HandoffDescription
	} else {
		h.Description = fmt.Sprintf("Handoff to %s", target.Name)
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// WithToolName overrides the default tool name.
func WithToolName(name string) HandoffOption {
	return func(h *Handoff) { h.ToolName = name }
}

// WithDescription overrides the default description.
func WithDescription(desc string) HandoffOption {
	return func(h *Handoff) { h.Description = desc }
}

// WithInputFilter sets an input filter for the handoff.
func WithInputFilter(fn func(input []RunItem, history []RunItem) []RunItem) HandoffOption {
	return func(h *Handoff) { h.InputFilter = fn }
}

// WithInputType sets the structured input schema the model must satisfy when
// triggering the handoff. The validated arguments are passed to OnHandoff.
func WithInputType(schema *OutputSchema) HandoffOption {
	return func(h *Handoff) { h.InputType = schema }
}

// WithOnHandoff sets a callback fired with the model-supplied handoff arguments
// when this handoff is triggered, before control transfers to the target agent.
func WithOnHandoff(fn func(ctx *RunContext, input json.RawMessage)) HandoffOption {
	return func(h *Handoff) { h.OnHandoff = fn }
}

// WithIsEnabled gates whether the handoff is exposed to the model for a given
// run context. A handoff with a predicate returning false is hidden.
func WithIsEnabled(fn func(ctx *RunContext) bool) HandoffOption {
	return func(h *Handoff) { h.IsEnabledFn = fn }
}

// ToTool returns a Tool that triggers this handoff when called.
func (h *Handoff) ToTool() Tool {
	return &handoffTool{handoff: h}
}

// handoffTool wraps a Handoff as a Tool.
type handoffTool struct {
	handoff *Handoff
}

func (t *handoffTool) Name() string        { return t.handoff.ToolName }
func (t *handoffTool) Description() string { return t.handoff.Description }

func (t *handoffTool) InputSchema() json.RawMessage {
	if t.handoff.InputType != nil {
		return t.handoff.InputType.Schema
	}
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *handoffTool) Execute(_ context.Context, _ json.RawMessage, _ string) (ToolResult, error) {
	return ToolResult{Content: fmt.Sprintf("Handing off to %s", t.handoff.Agent.Name)}, nil
}

func (t *handoffTool) IsReadOnly() bool { return true }
func (t *handoffTool) IsEnabled(ctx *RunContext) bool {
	if t.handoff != nil && t.handoff.IsEnabledFn != nil {
		return t.handoff.IsEnabledFn(ctx)
	}
	return true
}
func (t *handoffTool) NeedsApproval() bool { return false }
func (t *handoffTool) TimeoutSeconds() int { return 0 }
