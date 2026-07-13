package agent

import (
	"context"
	"encoding/json"
)

// Tool is the interface for all tools available to agents.
type Tool interface {
	Name() string
	Description() string
	InputSchema() json.RawMessage
	Execute(ctx context.Context, input json.RawMessage, workDir string) (ToolResult, error)
	IsReadOnly() bool
	IsEnabled(ctx *RunContext) bool
	NeedsApproval() bool
	TimeoutSeconds() int
}

// ToolTimeoutResultPreserver is implemented by context-aware tools that turn
// their own deadline into a complete, model-safe result. The runner normally
// replaces any result produced at the tool-policy deadline with
// ToolTimeoutError; this opt-in lets managed wait tools return a non-fatal
// progress snapshot instead. The result is preserved only when Execute returns
// no error and a non-error ToolResult.
type ToolTimeoutResultPreserver interface {
	PreserveResultOnTimeout() bool
}

// ToolAccessAdapter lets a host application provide a safer tool
// implementation for a specific access level.
type ToolAccessAdapter interface {
	ToolForAccess(level ToolAccessLevel) Tool
}

// MutatingToolEnabled reports whether a mutating tool may be considered
// enabled for the run's access context. The runner's exact-name filter remains
// the authorization boundary; this helper lets access-aware IsEnabled methods
// recognize the same host-trusted exception instead of rejecting it early.
func MutatingToolEnabled(ctx *RunContext, toolName string) bool {
	if ctx == nil || NormalizeToolAccessLevel(ctx.ToolAccessLevel) != ToolAccessLevelReadOnly {
		return true
	}
	for _, allowed := range ctx.Config.AllowedMutatingTools {
		if allowed != "" && allowed == toolName {
			return true
		}
	}
	return false
}

// ToolResult is the outcome of a tool execution.
type ToolResult struct {
	Content     string
	IsError     bool
	ShouldPause bool // When true, the runner breaks so the outer loop can handle the event.
}

// FunctionTool is a convenience tool built from a plain function.
type FunctionTool struct {
	ToolName        string
	ToolDescription string
	Schema          json.RawMessage
	Fn              func(ctx context.Context, input json.RawMessage) (string, error)
	ReadOnly        bool
	Approval        bool
	Timeout         int
}

func (t *FunctionTool) Name() string                 { return t.ToolName }
func (t *FunctionTool) Description() string          { return t.ToolDescription }
func (t *FunctionTool) InputSchema() json.RawMessage { return t.Schema }
func (t *FunctionTool) IsReadOnly() bool             { return t.ReadOnly }
func (t *FunctionTool) IsEnabled(ctx *RunContext) bool {
	return t.ReadOnly || MutatingToolEnabled(ctx, t.Name())
}
func (t *FunctionTool) NeedsApproval() bool { return t.Approval }
func (t *FunctionTool) TimeoutSeconds() int { return t.Timeout }

func (t *FunctionTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	result, err := t.Fn(ctx, input)
	if err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return ToolResult{Content: result}, nil
}

// policyToolWrapper adds host-supplied approval and timeout behavior to a tool.
type policyToolWrapper struct {
	inner    Tool
	approval bool
	timeout  int
}

// WrapWithPolicy wraps a tool with approval and timeout settings.
// If neither policy applies, it returns the original tool unchanged.
//
// Approval semantics: when approval=true, NeedsApproval() returns true even
// for tools whose own implementation hardcodes NeedsApproval()=false (notably
// the signal tools in pkg/agentsdk/tools/signal/*: finish, plan, present_plan,
// question). The wrapper's approval flag is therefore an *override*
// — hosts that opt these tools into approval gating via tool policy will see
// approval prompts despite the inner tool's default. The reverse is not true:
// approval=false leaves the inner tool's NeedsApproval() decision intact.
func WrapWithPolicy(t Tool, approval bool, timeout int) Tool {
	if !approval && timeout == 0 {
		return t
	}
	return &policyToolWrapper{inner: t, approval: approval, timeout: timeout}
}

func (w *policyToolWrapper) Name() string                 { return w.inner.Name() }
func (w *policyToolWrapper) Description() string          { return w.inner.Description() }
func (w *policyToolWrapper) InputSchema() json.RawMessage { return w.inner.InputSchema() }
func (w *policyToolWrapper) Execute(ctx context.Context, input json.RawMessage, workDir string) (ToolResult, error) {
	return w.inner.Execute(ctx, input, workDir)
}
func (w *policyToolWrapper) IsReadOnly() bool               { return w.inner.IsReadOnly() }
func (w *policyToolWrapper) IsEnabled(ctx *RunContext) bool { return w.inner.IsEnabled(ctx) }

func (w *policyToolWrapper) NeedsApproval() bool {
	if w.approval {
		return true
	}
	return w.inner.NeedsApproval()
}

func (w *policyToolWrapper) TimeoutSeconds() int {
	if w.timeout > 0 {
		return w.timeout
	}
	return w.inner.TimeoutSeconds()
}

func (w *policyToolWrapper) PreserveResultOnTimeout() bool {
	preserver, ok := w.inner.(ToolTimeoutResultPreserver)
	return ok && preserver.PreserveResultOnTimeout()
}

// ToolForAccess delegates to the inner tool's ToolAccessAdapter (if any) and
// re-wraps the adapted tool with the same approval/timeout policy. Without
// this delegation, hosts that supply a safer tool implementation per access
// level (e.g. a read-only Bash) would be silently bypassed once the policy
// wrapper was applied. See finding M10.
func (w *policyToolWrapper) ToolForAccess(level ToolAccessLevel) Tool {
	adapter, ok := w.inner.(ToolAccessAdapter)
	if !ok {
		return w
	}
	adapted := adapter.ToolForAccess(level)
	if adapted == nil {
		return nil
	}
	return &policyToolWrapper{inner: adapted, approval: w.approval, timeout: w.timeout}
}
