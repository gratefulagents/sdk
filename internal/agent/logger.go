package agent

import (
	"fmt"
	"log"
	"strings"
)

// LogLevel controls agent pod log verbosity.
type LogLevel int

const (
	LogLevelNormal LogLevel = iota
	LogLevelDebug
)

// AgentLogger provides structured, level-aware logging for the agent pod.
// Normal mode logs every event concisely (turns, tools, phases, model responses).
// Debug mode adds full tool inputs/outputs, instructions, and conversation items.
type AgentLogger struct {
	level LogLevel
}

// NewAgentLogger creates a logger at the given level.
func NewAgentLogger(level LogLevel) *AgentLogger {
	return &AgentLogger{level: level}
}

func (l *AgentLogger) IsDebug() bool { return l.level >= LogLevelDebug }

// Turn logs the start of a new LLM turn.
func (l *AgentLogger) Turn(turn int, agent, model string) {
	log.Printf("[turn] ===== TURN %d agent=%q model=%q =====", turn, agent, model)
}

// TurnEnd logs end of turn payload.
func (l *AgentLogger) TurnEnd(turn int) {
	log.Printf("[turn] ===== END TURN %d PAYLOAD =====", turn)
}

// Tools logs tool and handoff summary.
func (l *AgentLogger) Tools(toolNames []string, handoffNames []string, accessLevel ToolAccessLevel, phase string) {
	log.Printf("[turn] tools=%d %v", len(toolNames), toolNames)
	log.Printf("[turn] handoffs=%d %v", len(handoffNames), handoffNames)
	log.Printf("[turn] tool_access=%q phase=%q", accessLevel, phase)
}

// Instructions logs system instructions. Full dump in debug, length-only in normal.
func (l *AgentLogger) Instructions(instructions string) {
	log.Printf("[turn] instructions_len=%d", len(instructions))
	if l.IsDebug() {
		const chunkSize = 2000
		for offset := 0; offset < len(instructions); offset += chunkSize {
			end := offset + chunkSize
			if end > len(instructions) {
				end = len(instructions)
			}
			log.Printf("[turn] instructions[%d:%d]=%q", offset, end, instructions[offset:end])
		}
	}
}

// InputItems logs conversation items sent to the model.
// Normal: count + role summary. Debug: full content.
func (l *AgentLogger) InputItems(items []RunItem) {
	log.Printf("[turn] input_items=%d", len(items))
	for i, item := range items {
		switch item.Type {
		case RunItemMessage:
			role := "user"
			if item.Agent != nil {
				role = "assistant"
			}
			text := ""
			if item.Message != nil {
				text = item.Message.Text
			}
			if l.IsDebug() {
				if len(text) > 500 {
					text = text[:500] + "…"
				}
				log.Printf("[turn] input[%d] type=message role=%s text=%q", i, role, text)
			} else {
				log.Printf("[turn] input[%d] type=message role=%s len=%d", i, role, len(text))
			}
		case RunItemToolCall:
			if l.IsDebug() {
				inputPreview := string(item.ToolCall.Input)
				if len(inputPreview) > 300 {
					inputPreview = inputPreview[:300] + "…"
				}
				log.Printf("[turn] input[%d] type=tool_call name=%s id=%s input=%s", i, item.ToolCall.Name, item.ToolCall.ID, inputPreview)
			} else {
				log.Printf("[turn] input[%d] type=tool_call name=%s id=%s", i, item.ToolCall.Name, item.ToolCall.ID)
			}
		case RunItemToolOutput:
			if l.IsDebug() {
				outputPreview := item.ToolOutput.Content
				if len(outputPreview) > 300 {
					outputPreview = outputPreview[:300] + "…"
				}
				log.Printf("[turn] input[%d] type=tool_output call_id=%s is_error=%v output=%s", i, item.ToolOutput.CallID, item.ToolOutput.IsError, outputPreview)
			} else {
				log.Printf("[turn] input[%d] type=tool_output call_id=%s is_error=%v len=%d", i, item.ToolOutput.CallID, item.ToolOutput.IsError, len(item.ToolOutput.Content))
			}
		case RunItemCompaction:
			id := ""
			encryptedLen := 0
			if item.Compaction != nil {
				id = item.Compaction.ID
				encryptedLen = len(item.Compaction.EncryptedContent)
			}
			log.Printf("[turn] input[%d] type=compaction id=%s encrypted_len=%d", i, id, encryptedLen)
		default:
			log.Printf("[turn] input[%d] type=%v", i, item.Type)
		}
	}
}

// ToolExec logs a tool execution start.
func (l *AgentLogger) ToolExec(name, callID string, inputLen int) {
	log.Printf("[tool] exec name=%s id=%s input_len=%d", name, callID, inputLen)
}

// ToolResult logs a tool result. Full output in debug, length-only in normal.
func (l *AgentLogger) ToolResult(name, callID string, isError bool, output string) {
	if l.IsDebug() {
		preview := output
		if len(preview) > 1000 {
			preview = preview[:1000] + "…"
		}
		log.Printf("[tool] result name=%s id=%s is_error=%v output=%s", name, callID, isError, preview)
	} else {
		log.Printf("[tool] result name=%s id=%s is_error=%v output_len=%d", name, callID, isError, len(output))
	}
}

// Event logs an EventStream event to stdout.
func (l *AgentLogger) Event(ev ContentEvent) {
	switch ev.Type {
	case "assistant_text":
		preview := ev.Message
		if len(preview) > 200 {
			preview = preview[:200] + "…"
		}
		log.Printf("[event] assistant_text len=%d preview=%q", len(ev.Message), preview)
	case "assistant_thinking":
		log.Printf("[event] assistant_thinking len=%d", len(ev.Message))
	case "tool_start":
		log.Printf("[event] tool_start tool=%s id=%s agent=%s", ev.Tool, ev.ToolUseID, ev.AgentName)
	case "tool_end":
		log.Printf("[event] tool_end tool=%s id=%s is_error=%v agent=%s", ev.Tool, ev.ToolUseID, ev.IsError, ev.AgentName)
	case "phase_transition":
		log.Printf("[event] phase_transition name=%s msg=%q", ev.Phase, ev.Message)
	case "subagent_status":
		log.Printf("[event] subagent_status task=%s status=%s type=%s model=%s msg=%q", ev.TaskID, ev.Status, ev.SubagentType, ev.SubagentModel, ev.Message)
	case "session_end":
		log.Printf("[event] session_end status=%s", ev.Status)
	case "llm_attempt":
		errText := ev.Output
		if len(errText) > 1000 {
			errText = errText[:1000] + "…"
		}
		if errText != "" {
			log.Printf("[event] llm_attempt status=%s model=%s provider=%s reason=%s latency_ms=%d error=%q", ev.AttemptStatus, ev.Model, ev.Provider, ev.FailureKind, ev.AttemptLatencyMs, errText)
		} else {
			log.Printf("[event] llm_attempt status=%s model=%s provider=%s latency_ms=%d", ev.AttemptStatus, ev.Model, ev.Provider, ev.AttemptLatencyMs)
		}
	default:
		log.Printf("[event] %s", ev.Type)
	}
}

// Debugf logs only in debug mode.
func (l *AgentLogger) Debugf(format string, args ...any) {
	if l.IsDebug() {
		log.Printf("[debug] "+format, args...)
	}
}

// Infof always logs.
func (l *AgentLogger) Infof(format string, args ...any) {
	log.Printf(format, args...)
}

// Phase logs a phase transition.
func (l *AgentLogger) Phase(name, kind, role string) {
	log.Printf("[phase] → %s (kind=%s role=%s)", name, kind, role)
}

// ModelResponse logs a model response summary.
func (l *AgentLogger) ModelResponse(model string, textCount, toolCount, thinkingCount int, inputTokens, outputTokens int64, stopReason string) {
	log.Printf("[model] response model=%s text=%d tools=%d thinking=%d usage=in:%d/out:%d stop=%s",
		model, textCount, toolCount, thinkingCount, inputTokens, outputTokens, stopReason)
}

// ParallelToolCalls logs parallel tool execution.
func (l *AgentLogger) ParallelToolCalls(names []string) {
	log.Printf("[tool] executing %d in parallel: %v", len(names), names)
}

// AutoContinue logs an auto-continue event.
func (l *AgentLogger) AutoContinue(count int, reason string) {
	log.Printf("[auto] continue %d: %s", count, reason)
}

// Warn logs a warning.
func (l *AgentLogger) Warn(msg string) {
	log.Printf("WARN: %s", msg)
}

// Warnf logs a formatted warning.
func (l *AgentLogger) Warnf(format string, args ...any) {
	log.Printf("WARN: "+format, args...)
}

// Error logs an error.
func (l *AgentLogger) Error(msg string) {
	log.Printf("ERROR: %s", msg)
}

// Errorf logs a formatted error.
func (l *AgentLogger) Errorf(format string, args ...any) {
	log.Printf("ERROR: "+format, args...)
}

// FormatToolNames formats a list of tool names for compact logging.
func FormatToolNames(names []string) string {
	return fmt.Sprintf("[%s]", strings.Join(names, " "))
}
