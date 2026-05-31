package agentsdk

import (
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultRecentConversationLimit = 8
	DefaultContextMessageCharLimit = 1200
	DefaultContextSummaryCharLimit = 320

	UserMessageModeEnqueue   = "enqueue"
	UserMessageModeImmediate = "immediate"
)

// ConversationMessage is the SDK-native message shape used by loop context
// builders. Hosts can map their durable message rows into this type.
type ConversationMessage struct {
	ID      int64
	Role    string
	Content string
	Images  []ImageAttachment
}

// BuildConversationTail selects recent durable conversation context while
// respecting a history floor and excluding the current user message.
func BuildConversationTail(messages []ConversationMessage, state WorkingState, excludeMessageID int64, limit int) []RunItem {
	if limit <= 0 {
		limit = DefaultRecentConversationLimit
	}

	filtered := make([]ConversationMessage, 0, len(messages))
	for _, msg := range messages {
		if msg.ID <= state.HistoryFloorMessageID {
			continue
		}
		if excludeMessageID > 0 && msg.ID == excludeMessageID {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		msg.Content = TruncateContextText(content, DefaultContextMessageCharLimit)
		filtered = append(filtered, msg)
	}
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}

	items := make([]RunItem, 0, len(filtered))
	for _, msg := range filtered {
		item := RunItem{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: msg.Content},
		}
		switch msg.Role {
		case "assistant":
			item.Agent = &Agent{Name: "assistant-summary"}
		case "system":
			item.Agent = &Agent{Name: "system-summary"}
		}
		items = append(items, item)
	}
	return items
}

// BuildWorkingStateContext formats compact, durable loop state for prompt
// injection.
func BuildWorkingStateContext(state WorkingState) string {
	var lines []string
	if state.Goal != "" {
		lines = append(lines, "Current objective: "+TruncateContextText(state.Goal, DefaultContextSummaryCharLimit))
	}
	if state.CurrentMode != "" {
		modeLine := "Mode: " + state.CurrentMode
		if state.CurrentPhase != "" {
			modeLine += fmt.Sprintf(" (phase: %s)", state.CurrentPhase)
		}
		lines = append(lines, modeLine)
	} else if state.CurrentPhase != "" {
		lines = append(lines, "Phase: "+state.CurrentPhase)
	}
	if state.CurrentStep != "" {
		lines = append(lines, "Current step: "+TruncateContextText(state.CurrentStep, DefaultContextSummaryCharLimit))
	}
	if state.LastUserMessage != "" && state.LastUserMessage != state.Goal {
		lines = append(lines, "Latest user direction: "+TruncateContextText(state.LastUserMessage, DefaultContextSummaryCharLimit))
	}
	if state.LastAssistantSummary != "" {
		lines = append(lines, "Latest assistant summary: "+TruncateContextText(state.LastAssistantSummary, DefaultContextSummaryCharLimit))
	}

	if len(state.RecentTurnSummaries) > 0 {
		recent := state.RecentTurnSummaries
		if len(recent) > 4 {
			recent = recent[len(recent)-4:]
		}
		progress := make([]string, 0, len(recent))
		for _, summary := range recent {
			progress = append(progress, TruncateContextText(summary, DefaultContextSummaryCharLimit))
		}
		lines = append(lines, "Recent progress:\n- "+strings.Join(progress, "\n- "))
	}

	if len(lines) == 0 {
		return ""
	}
	return "## Durable Working State\n" + strings.Join(lines, "\n")
}

// DeriveWorkingStateGoal preserves the effective prompt for shorthand approval
// replies such as "approve" or "request changes: ...".
func DeriveWorkingStateGoal(rawReply, effectivePrompt string) string {
	rawReply = strings.TrimSpace(rawReply)
	effectivePrompt = strings.TrimSpace(effectivePrompt)
	if rawReply == "" {
		return effectivePrompt
	}

	lower := strings.ToLower(rawReply)
	switch {
	case lower == "approve", lower == "deny", lower == "request changes", lower == "request_changes":
		if effectivePrompt != "" {
			return effectivePrompt
		}
	case strings.HasPrefix(lower, "deny:"), strings.HasPrefix(lower, "request changes:"), strings.HasPrefix(lower, "request_changes:"):
		if effectivePrompt != "" {
			return effectivePrompt
		}
	}

	return rawReply
}

// BuildAssistantTurnSummary creates a compact durable summary from one runner
// turn's output items.
func BuildAssistantTurnSummary(items []RunItem) string {
	assistantMessages := collectUniqueBullets(items, 2, func(item RunItem) string {
		if item.Type != RunItemMessage || item.Message == nil || item.Agent == nil {
			return ""
		}
		return TruncateContextText(item.Message.Text, 220)
	})
	toolSummary := SummarizeTurnToolCalls(items, 4)
	successes := collectUniqueBullets(items, 2, func(item RunItem) string {
		if item.Type != RunItemToolOutput || item.ToolOutput == nil || item.ToolOutput.IsError {
			return ""
		}
		return TruncateContextText(item.ToolOutput.Content, 120)
	})
	issues := collectUniqueBullets(items, 2, func(item RunItem) string {
		if item.Type != RunItemToolOutput || item.ToolOutput == nil || !item.ToolOutput.IsError {
			return ""
		}
		return TruncateContextText(item.ToolOutput.Content, 120)
	})

	var parts []string
	if len(assistantMessages) > 0 {
		parts = append(parts, strings.Join(assistantMessages, "\n"))
	}
	if len(toolSummary) > 0 {
		parts = append(parts, "Tools: "+strings.Join(toolSummary, ", "))
	}
	if len(successes) > 0 && len(assistantMessages) == 0 {
		parts = append(parts, "Key results: "+strings.Join(successes, " | "))
	}
	if len(issues) > 0 {
		parts = append(parts, "Issues: "+strings.Join(issues, " | "))
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// SummarizeTurnToolCalls counts tool calls in one runner turn.
func SummarizeTurnToolCalls(items []RunItem, limit int) []string {
	counts := map[string]int{}
	for _, item := range items {
		if item.Type == RunItemToolCall && item.ToolCall != nil {
			counts[item.ToolCall.Name]++
		}
	}
	if len(counts) == 0 {
		return nil
	}

	type pair struct {
		name  string
		count int
	}
	pairs := make([]pair, 0, len(counts))
	for name, count := range counts {
		pairs = append(pairs, pair{name: name, count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count == pairs[j].count {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].count > pairs[j].count
	})

	if limit <= 0 || limit > len(pairs) {
		limit = len(pairs)
	}
	out := make([]string, 0, limit)
	for _, pair := range pairs[:limit] {
		out = append(out, fmt.Sprintf("%s x %d", pair.name, pair.count))
	}
	return out
}

func collectUniqueBullets(items []RunItem, limit int, fn func(RunItem) string) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, limit)
	for _, item := range items {
		text := strings.TrimSpace(fn(item))
		if text == "" {
			continue
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		out = append(out, text)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// TruncateContextText normalizes whitespace and trims context snippets.
func TruncateContextText(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}

// SelectNextUserMessage picks the next message to handle, prioritizing
// unconsumed immediate messages while preserving queued messages.
func SelectNextUserMessage(messages []UserMessage, consumedImmediate map[int64]struct{}) (UserMessage, bool, int64, bool) {
	skipCursor := leadingSkippableCursor(messages, consumedImmediate)

	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" && len(msg.Images) == 0 {
			continue
		}
		if msg.Mode != UserMessageModeImmediate {
			continue
		}
		if _, seen := consumedImmediate[msg.ID]; seen {
			continue
		}
		return msg, true, skipCursor, true
	}

	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" && len(msg.Images) == 0 {
			continue
		}
		if msg.Mode == UserMessageModeImmediate {
			if _, seen := consumedImmediate[msg.ID]; seen {
				continue
			}
		}
		return msg, true, skipCursor, false
	}

	return UserMessage{}, false, skipCursor, false
}

func leadingSkippableCursor(messages []UserMessage, consumedImmediate map[int64]struct{}) int64 {
	var skipCursor int64
	for _, msg := range messages {
		content := strings.TrimSpace(msg.Content)
		if content == "" && len(msg.Images) == 0 {
			skipCursor = msg.ID
			continue
		}
		if msg.Mode == UserMessageModeImmediate {
			if _, seen := consumedImmediate[msg.ID]; seen {
				skipCursor = msg.ID
				continue
			}
		}
		break
	}
	return skipCursor
}

// CollectImmediateRunItems converts unconsumed immediate user messages to run
// items and marks them consumed.
func CollectImmediateRunItems(messages []UserMessage, consumedImmediate map[int64]struct{}) ([]RunItem, int64) {
	var (
		items      []RunItem
		lastCursor int64
	)
	for _, msg := range messages {
		lastCursor = msg.ID
		if msg.Mode != UserMessageModeImmediate {
			continue
		}
		if _, seen := consumedImmediate[msg.ID]; seen {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" && len(msg.Images) == 0 {
			continue
		}
		consumedImmediate[msg.ID] = struct{}{}
		items = append(items, RunItem{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: content, Images: msg.Images},
		})
	}
	return items, lastCursor
}
