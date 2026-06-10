package agent

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// PlatformHooks bridges the Runner lifecycle to RunProgress and EventWriter.
// Content events go to EventWriter; structural/metrics data goes to OTel spans
// through TracingProcessor and snapshots through RunProgress.
type PlatformHooks struct {
	Tracker     *ProgressTracker
	EventStream *EventStream
	Turn        int // current conversation turn, set by caller
	// Activity optionally tracks file operations for parent visibility.
	// Set on child hooks to let the parent query sub-agent activity.
	Activity *SubAgentActivity
	// toolStart tracks when each tool began for duration calculation.
	// Protected by mu since tools execute in parallel goroutines.
	mu        sync.Mutex
	toolStart map[string]time.Time
}

// NewPlatformHooks creates a PlatformHooks bridging to the given tracker and event stream.
func NewPlatformHooks(tracker *ProgressTracker, es *EventStream) *PlatformHooks {
	return &PlatformHooks{
		Tracker:     tracker,
		EventStream: es,
		toolStart:   make(map[string]time.Time),
	}
}

func (h *PlatformHooks) OnAgentStart(_ *RunContext, agent *Agent) {
	h.Tracker.RecordLifecycleEvent("agent_start", fmt.Sprintf("Agent %q started", agent.Name))
}

func (h *PlatformHooks) OnAgentEnd(_ *RunContext, agent *Agent, _ any) {
	h.Tracker.RecordLifecycleEvent("agent_end", fmt.Sprintf("Agent %q ended", agent.Name))
}

func (h *PlatformHooks) OnHandoff(_ *RunContext, from *Agent, to *Agent) {
	h.Tracker.RecordLifecycleEvent("handoff", fmt.Sprintf("Handoff from %q to %q", from.Name, to.Name))
}

func (h *PlatformHooks) OnToolStart(runCtx *RunContext, agent *Agent, tool Tool, call ToolCallData) {
	parentCallID := ParentCallIDFromContext(runCtx.Context())
	toolCallID := ToolCallIDFromContext(runCtx.Context())
	h.mu.Lock()
	h.toolStart[toolCallID] = time.Now()
	h.mu.Unlock()
	inputSummary := extractToolInputSummary(tool.Name(), call.Input)
	h.Tracker.RecordToolUse(tool.Name(), inputSummary, toolCallID, h.Turn, string(call.Input), agent.Name, parentCallID)
	if h.EventStream != nil {
		h.EventStream.SetStep(h.Tracker.CurrentStep())
		h.EventStream.EmitToolStart(tool.Name(), toolCallID, parentCallID, inputSummary, agent.Name, string(call.Input))
	}
	if h.Activity != nil {
		h.Activity.RecordToolStart(tool.Name(), inputSummary)
	}
}

func (h *PlatformHooks) OnToolEnd(runCtx *RunContext, agent *Agent, tool Tool, call ToolCallData, result ToolResult) {
	parentCallID := ParentCallIDFromContext(runCtx.Context())
	toolCallID := ToolCallIDFromContext(runCtx.Context())
	h.mu.Lock()
	var durationMS int64
	if start, ok := h.toolStart[toolCallID]; ok {
		durationMS = time.Since(start).Milliseconds()
		delete(h.toolStart, toolCallID)
	}
	h.mu.Unlock()
	inputSummary := extractToolInputSummary(tool.Name(), call.Input)
	h.Tracker.RecordToolResult(toolCallID, tool.Name(), result.Content, result.IsError, durationMS, agent.Name, parentCallID)
	if h.EventStream != nil {
		if tool.Name() == "set_phase" {
			var payload struct {
				Phase string `json:"phase"`
			}
			if json.Unmarshal(call.Input, &payload) == nil {
				h.EventStream.SetPhase(payload.Phase)
			}
		}
		h.EventStream.SetStep(h.Tracker.CurrentStep())
		h.EventStream.EmitToolEnd(tool.Name(), toolCallID, parentCallID, result.IsError, agent.Name, result.Content, durationMS)
	}
	if h.Activity != nil {
		h.Activity.RecordToolEnd(tool.Name(), inputSummary, result.IsError, durationMS)
	}
}

// extractToolInputSummary returns a human-readable summary for a tool call.
func extractToolInputSummary(toolName string, input json.RawMessage) string {
	switch toolName {
	case "Edit", "Write":
		return ExtractFilePath(input)
	case "read_file":
		return ExtractPath(input)
	case "Bash":
		return ExtractBashCommand(input)
	case "AskUserQuestion":
		return ExtractAskUserQuestion(input)
	default:
		return ""
	}
}

func (h *PlatformHooks) OnLLMStart(_ *RunContext, _ *Agent) {
	// No-op: the model call start isn't separately tracked in our activity log.
}

func (h *PlatformHooks) OnLLMEnd(_ *RunContext, agent *Agent, response *ModelResponse) {
	if response == nil {
		return
	}

	// Accumulate cost and token usage in the tracker so that
	// periodic progress writes (to Postgres and CRD) stay current.
	h.Tracker.RecordLLMUsage(agent.Model, response.CostUSD, response.Usage)

	// Record assistant text and thinking from response items.
	for _, item := range response.Items {
		switch item.Type {
		case RunItemMessage:
			if item.Message != nil && item.Message.Text != "" {
				h.Tracker.RecordAssistantText(item.Message.Text)
				if h.EventStream != nil {
					h.EventStream.EmitText(item.Message.Text, agent.Name)
				}
			}
		case RunItemReasoning:
			if item.Reasoning != nil && item.Reasoning.Text != "" {
				h.Tracker.RecordAssistantThinking(item.Reasoning.Text)
				if h.EventStream != nil {
					h.EventStream.EmitThinking(item.Reasoning.Text, agent.Name)
				}
			}
		}
	}

	// Record usage in the tracker's model usage map via a lifecycle event.
	usage := response.Usage
	detail, _ := json.Marshal(map[string]any{
		"model":         agent.Model,
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
	})
	h.Tracker.RecordLifecycleEvent("llm_end", string(detail))
}
