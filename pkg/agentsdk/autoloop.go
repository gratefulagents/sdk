package agentsdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
)

const (
	DefaultMaxAutoLoops = 200

	autoTrackerMaxRecentTools  = 10
	autoTrackerMaxRecentErrors = 5

	// Reasoning-heavy models (e.g. claude-fable-5) legitimately produce a few
	// consecutive planning turns with no tool calls before acting; the
	// breaker allows five such turns and the strong warning starts at three.
	defaultCBMaxNoToolTurns   = 5
	cbNoToolWarningTurns      = 3
	defaultCBMaxSameErrors    = 5
	defaultCBMaxRepeatedCycle = 6
)

// AutoTracker tracks autonomous-loop progress for smart continuation and
// circuit breakers.
type AutoTracker struct {
	consecutiveNoToolTurns int
	recentToolCalls        []string
	recentToolSignatures   []string
	recentErrors           []string
	toolCallCount          int
}

// CircuitBreakerResult reports whether an autonomous loop should pause.
type CircuitBreakerResult struct {
	Tripped bool
	Reason  string
}

// Update analyzes the latest runner result items and updates tracker state.
func (t *AutoTracker) Update(newItems []RunItem) {
	if t == nil {
		return
	}
	var turnToolCalls []string
	var turnToolSignatures []string
	var turnErrors []string
	for _, item := range newItems {
		if item.Type == RunItemToolCall && item.ToolCall != nil {
			turnToolCalls = append(turnToolCalls, item.ToolCall.Name)
			turnToolSignatures = append(turnToolSignatures, toolCallSignature(item.ToolCall.Name, item.ToolCall.Input))
		}
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.IsError {
			msg := item.ToolOutput.Content
			if len(msg) > 200 {
				msg = msg[:200]
			}
			turnErrors = append(turnErrors, msg)
		}
	}

	if len(turnToolCalls) == 0 {
		t.consecutiveNoToolTurns++
	} else {
		t.consecutiveNoToolTurns = 0
	}
	t.toolCallCount += len(turnToolCalls)

	for i, call := range turnToolCalls {
		t.recentToolCalls = append(t.recentToolCalls, call)
		t.recentToolSignatures = append(t.recentToolSignatures, turnToolSignatures[i])
		if len(t.recentToolCalls) > autoTrackerMaxRecentTools {
			t.recentToolCalls = t.recentToolCalls[1:]
			t.recentToolSignatures = t.recentToolSignatures[1:]
		}
	}
	for _, errText := range turnErrors {
		t.recentErrors = append(t.recentErrors, errText)
		if len(t.recentErrors) > autoTrackerMaxRecentErrors {
			t.recentErrors = t.recentErrors[1:]
		}
	}
}

func (t *AutoTracker) ConsecutiveNoToolTurns() int {
	if t == nil {
		return 0
	}
	return t.consecutiveNoToolTurns
}

func (t *AutoTracker) ToolCallCount() int {
	if t == nil {
		return 0
	}
	return t.toolCallCount
}

func (t *AutoTracker) consecutiveSameError() (int, string) {
	if t == nil || len(t.recentErrors) == 0 {
		return 0, ""
	}
	last := t.recentErrors[len(t.recentErrors)-1]
	count := 0
	for i := len(t.recentErrors) - 1; i >= 0; i-- {
		if t.recentErrors[i] == last {
			count++
		} else {
			break
		}
	}
	return count, last
}

// detectRepetitiveCycle reports whether the recent tool calls form a repeating
// cycle of *identical* calls. Calls are compared on an argument-aware signature
// (tool name + input), so ordinary exploration - reading several different
// files, or alternating grep/read across distinct targets - is not mistaken for
// a loop. The returned slice contains the tool names of the detected cycle for
// human-readable reporting.
func (t *AutoTracker) detectRepetitiveCycle() (bool, []string) {
	if t == nil {
		return false, nil
	}
	sigs := t.recentToolSignatures
	names := t.recentToolCalls
	if len(sigs) != len(names) {
		return false, nil
	}
	for cycleLen := 2; cycleLen <= 3; cycleLen++ {
		needed := cycleLen * 2
		if len(sigs) < needed {
			continue
		}
		tailSigs := sigs[len(sigs)-needed:]
		tailNames := names[len(names)-needed:]
		cycle := tailSigs[:cycleLen]
		match := true
		for i := 0; i < needed; i++ {
			if tailSigs[i] != cycle[i%cycleLen] {
				match = false
				break
			}
		}
		if match {
			return true, tailNames[:cycleLen]
		}
	}
	return false, nil
}

// toolCallSignature returns a compact, argument-aware identity for a tool call
// so that two calls are considered "the same" only when both the tool name and
// the (whitespace-normalized) input match.
func toolCallSignature(name string, input json.RawMessage) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	if len(input) > 0 {
		var buf bytes.Buffer
		if err := json.Compact(&buf, input); err == nil {
			_, _ = h.Write(buf.Bytes())
		} else {
			_, _ = h.Write(input)
		}
	}
	return fmt.Sprintf("%s:%x", name, h.Sum64())
}

// CheckCircuitBreakers reports hard-stop autonomous loop conditions.
func (t *AutoTracker) CheckCircuitBreakers() CircuitBreakerResult {
	if t == nil {
		return CircuitBreakerResult{}
	}
	if t.consecutiveNoToolTurns >= defaultCBMaxNoToolTurns {
		return CircuitBreakerResult{
			Tripped: true,
			Reason:  fmt.Sprintf("Agent stalled - no tool calls in %d consecutive turns", t.consecutiveNoToolTurns),
		}
	}
	errCount, errMsg := t.consecutiveSameError()
	if errCount >= defaultCBMaxSameErrors {
		return CircuitBreakerResult{
			Tripped: true,
			Reason:  fmt.Sprintf("Agent stuck - same error repeated %d times: %s", errCount, truncateTitle(errMsg, 100)),
		}
	}
	if cycling, cycle := t.detectRepetitiveCycle(); cycling {
		cycleCount := len(t.recentToolCalls) / len(cycle)
		if cycleCount >= defaultCBMaxRepeatedCycle/len(cycle) {
			return CircuitBreakerResult{
				Tripped: true,
				Reason:  fmt.Sprintf("Agent stuck in tool loop - pattern [%s] repeated %d times", strings.Join(cycle, "->"), cycleCount),
			}
		}
	}
	return CircuitBreakerResult{}
}

// BuildSmartNudge generates a context-aware autonomous continuation prompt.
func BuildSmartNudge(t *AutoTracker, phase string) string {
	if t == nil {
		t = &AutoTracker{}
	}
	phaseHint := ""
	if phase != "" {
		phaseHint = fmt.Sprintf(" You are in the %s phase.", phase)
	}

	if cycling, cycle := t.detectRepetitiveCycle(); cycling {
		return fmt.Sprintf("[SYSTEM] Detected repetitive tool pattern: [%s]. You appear stuck in a loop. Step back, reassess your approach, and try something fundamentally different. If the current subtask is blocked, skip it and move on to other work.%s",
			strings.Join(cycle, " -> "), autonomousFinishGuidance())
	}

	errCount, errMsg := t.consecutiveSameError()
	if errCount >= 3 {
		return fmt.Sprintf("[SYSTEM] You have hit the same error %d times: \"%s\". Do NOT retry the same approach. Try a fundamentally different strategy, or skip this subtask and move on.%s",
			errCount, truncateTitle(errMsg, 120), autonomousFinishGuidance())
	}

	if t.consecutiveNoToolTurns >= cbNoToolWarningTurns {
		return fmt.Sprintf("[SYSTEM] WARNING: You have produced text without tool calls for %d consecutive turns. In autonomous mode, every turn MUST include at least one tool call. Do NOT output only text.", t.consecutiveNoToolTurns) + autonomousFinishGuidance()
	}
	if t.consecutiveNoToolTurns >= 1 {
		return "[SYSTEM] You produced text without tool calls. In autonomous mode, every turn must include at least one tool call." + autonomousFinishGuidance()
	}
	return fmt.Sprintf("[SYSTEM] Continue.%s %d tool calls so far. Use tools to make progress.%s", phaseHint, t.toolCallCount, autonomousFinishGuidance())
}

func autonomousFinishGuidance() string {
	return " Finish is a hard stop, not a progress report: if you named any backlog item, TODO, follow-up, remaining risk, or next step, call a tool for the top item now. Call finish only when no actionable work remains or a concrete external blocker prevents useful tool progress."
}

func truncateTitle(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}
