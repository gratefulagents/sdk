package agent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	defaultOutputReserveTokens = 16384
	requestSafetyBufferTokens  = 8192
)

// MaybeCompactRunItems reduces history size while preserving the original task
// framing and the most recent turn context.
func MaybeCompactRunItems(items []RunItem, cfg CompactionConfig) ([]RunItem, int, int, bool, string) {
	cfg = cfg.normalized()
	if !cfg.Enabled || len(items) == 0 {
		return items, 0, 0, false, "disabled"
	}

	before := estimateRunItemsTokens(items)
	if before <= cfg.TriggerTokens {
		return items, before, before, false, "below-threshold"
	}

	protectedPrefix := selectInitialUserMessageIndices(items, cfg.PreserveInitialUserMessages)
	maxRecent := minInt(cfg.PreserveRecentItems, len(items))
	if maxRecent < 1 {
		maxRecent = minInt(len(items), 1)
	}

	var (
		bestItems  []RunItem
		bestAfter  = before
		bestOK     bool
		bestReason = "no-removable-history"
	)
	for recent := maxRecent; recent >= 1; recent-- {
		compacted, after, ok, reason := compactRunItemsToRecentLimit(items, protectedPrefix, recent, cfg.SummaryBulletLimit, cfg.TargetTokens)
		if !ok {
			if bestReason == "no-removable-history" {
				bestReason = reason
			}
			continue
		}
		if !bestOK || after < bestAfter {
			bestItems = compacted
			bestAfter = after
			bestOK = true
		}
		if after <= cfg.TargetTokens {
			return compacted, before, after, true, ""
		}
	}
	if !bestOK {
		return items, before, before, false, bestReason
	}
	return bestItems, before, bestAfter, true, ""
}

// MaybeCompactRunItemsForRequest applies compaction against the estimated full
// request size, not just the persisted run items. Instructions, tool schemas,
// output reserve, and a safety buffer consume the same model context window.
func MaybeCompactRunItemsForRequest(items []RunItem, cfg CompactionConfig, requestOverheadTokens int) ([]RunItem, int, int, bool, string) {
	cfg = cfg.normalized()
	if !cfg.Enabled || len(items) == 0 {
		return items, 0, 0, false, "disabled"
	}
	if requestOverheadTokens < 0 {
		requestOverheadTokens = 0
	}

	beforeItems := estimateRunItemsTokens(items)
	beforeTotal := beforeItems + requestOverheadTokens
	if beforeTotal <= cfg.TriggerTokens {
		return items, beforeTotal, beforeTotal, false, "below-threshold"
	}

	adjusted := cfg
	adjusted.TriggerTokens = maxInt(1, cfg.TriggerTokens-requestOverheadTokens)
	adjusted.TargetTokens = maxInt(1, cfg.TargetTokens-requestOverheadTokens)
	compacted, _, afterItems, ok, reason := MaybeCompactRunItems(items, adjusted)
	if !ok {
		return items, beforeTotal, beforeTotal, false, reason
	}
	return compacted, beforeTotal, afterItems + requestOverheadTokens, true, ""
}

func ExtractCompactionSummary(items []RunItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Type != RunItemMessage || item.Message == nil || item.Agent == nil {
			continue
		}
		if item.Agent.Name != "context-summary" {
			continue
		}
		text := strings.TrimSpace(item.Message.Text)
		if strings.HasPrefix(text, "[COMPACTED HISTORY SUMMARY]") {
			return text
		}
	}
	return ""
}

// MaybeCompactHandoffInput applies a narrower compaction policy for handoff payloads.
func MaybeCompactHandoffInput(items []RunItem, cfg HandoffHistoryConfig) ([]RunItem, int, int, bool, string) {
	cfg = cfg.normalized()
	if !cfg.Enabled {
		return items, 0, 0, false, "disabled"
	}
	return MaybeCompactRunItems(items, CompactionConfig{
		Enabled:                     true,
		TriggerTokens:               cfg.MaxTokens,
		TargetTokens:                cfg.TargetTokens,
		PreserveRecentItems:         maxInt(cfg.PreserveRecentItems, 2),
		PreserveInitialUserMessages: 1,
		SummaryBulletLimit:          cfg.SummaryBulletLimit,
	})
}

func estimateRunItemsTokens(items []RunItem) int {
	total := 0
	for _, item := range items {
		switch item.Type {
		case RunItemMessage:
			if item.Message != nil {
				total += estimateStringTokens(item.Message.Text) + 8
			}
		case RunItemToolCall:
			if item.ToolCall != nil {
				total += estimateStringTokens(item.ToolCall.Name) + estimateStringTokens(string(item.ToolCall.Input)) + 16
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil {
				total += estimateStringTokens(item.ToolOutput.Content) + 12
			}
		case RunItemReasoning:
			if item.Reasoning != nil {
				total += estimateStringTokens(item.Reasoning.Text) + 8
			}
		case RunItemCompaction:
			if item.Compaction != nil {
				total += estimateStringTokens(item.Compaction.EncryptedContent) + 8
			}
		case RunItemHandoffCall:
			if item.HandoffCall != nil {
				total += estimateStringTokens(item.HandoffCall.FromAgent) + estimateStringTokens(item.HandoffCall.ToAgent) + 8
			}
		case RunItemHandoffOutput:
			if item.HandoffOutput != nil {
				total += estimateStringTokens(item.HandoffOutput.FromAgent) + estimateStringTokens(item.HandoffOutput.ToAgent) + 8
			}
		case RunItemToolApproval:
			if item.ToolApproval != nil {
				total += estimateStringTokens(item.ToolApproval.ToolName) + estimateStringTokens(string(item.ToolApproval.Input)) + 8
			}
		default:
			total += 8
		}
	}
	return total
}

func estimateModelRequestOverheadTokens(instructions string, tools []Tool, settings ModelSettings) int {
	total := estimateStringTokens(instructions) + 8
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		total += estimateStringTokens(tool.Name())
		total += estimateStringTokens(tool.Description())
		total += estimateStringTokens(string(tool.InputSchema()))
		total += 32
	}
	total += outputReserveTokens(settings)
	total += requestSafetyBufferTokens
	return total
}

func outputReserveTokens(settings ModelSettings) int {
	reserve := settings.MaxTokens
	if reserve <= 0 {
		reserve = defaultOutputReserveTokens
	}
	if settings.ThinkingBudget > reserve {
		reserve = settings.ThinkingBudget
	}
	return reserve
}

func estimateStringTokens(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// Rough token estimate: ~4 chars/token plus a small floor.
	return len([]rune(s))/4 + 1
}

func selectInitialUserMessageIndices(items []RunItem, limit int) []int {
	var indices []int
	for idx, item := range items {
		if item.Type != RunItemMessage || item.Message == nil || item.Agent != nil {
			continue
		}
		text := strings.TrimSpace(item.Message.Text)
		if text == "" || strings.HasPrefix(text, "[SYSTEM]") || strings.HasPrefix(text, "[PHASE TRANSITION") {
			continue
		}
		indices = append(indices, idx)
		if len(indices) >= limit {
			break
		}
	}
	return indices
}

func summarizeCompactedHistory(items []RunItem, bulletLimit int) string {
	bulletLimit = maxInt(bulletLimit, 1)
	existingSummary := latestExistingCompactionSummary(items)
	compactedItems := excludeCompactionSummaryItems(items)
	if len(compactedItems) == 0 {
		compactedItems = items
	}
	newSummary := summarizeCompactedMessages(compactedItems, bulletLimit)
	if existingSummary != "" {
		newSummary = mergeCompactionSummaries(existingSummary, newSummary)
	}
	return "[COMPACTED HISTORY SUMMARY]\n" + newSummary
}

func summarizeCompactedMessages(items []RunItem, bulletLimit int) string {
	bulletLimit = maxInt(bulletLimit, 1)
	userBullets := collectRecentUniqueBullets(items, bulletLimit, func(item RunItem) string {
		if item.Type != RunItemMessage || item.Message == nil || item.Agent != nil {
			return ""
		}
		text := strings.TrimSpace(item.Message.Text)
		if text == "" || strings.HasPrefix(text, "[SYSTEM]") || strings.HasPrefix(text, "[PHASE TRANSITION") {
			return ""
		}
		return truncateLine(text, 160)
	})
	pendingBullets := collectRecentUniqueBullets(items, bulletLimit, func(item RunItem) string {
		text := runItemTextForSummary(item)
		if text == "" {
			return ""
		}
		lower := strings.ToLower(text)
		if !strings.Contains(lower, "todo") &&
			!strings.Contains(lower, "next") &&
			!strings.Contains(lower, "pending") &&
			!strings.Contains(lower, "follow up") &&
			!strings.Contains(lower, "remaining") {
			return ""
		}
		return truncateLine(text, 160)
	})
	toolNames := summarizeToolsMentioned(items)
	fileBullets := summarizeReferencedPaths(items, 8)
	currentWork := inferCurrentWork(items)
	timelineBullets := summarizeKeyTimeline(items, 0)

	lines := []string{
		"Conversation summary:",
		"- " + summarizeCompactionScope(items),
	}
	if len(toolNames) > 0 {
		lines = append(lines, "- Tools mentioned: "+strings.Join(toolNames, ", ")+".")
	}
	appendClawSummarySection(&lines, "Recent user requests", userBullets)
	appendClawSummarySection(&lines, "Pending work", pendingBullets)
	if len(fileBullets) > 0 {
		lines = append(lines, "- Key files referenced: "+strings.Join(fileBullets, ", ")+".")
	}
	if currentWork != "" {
		lines = append(lines, "- Current work: "+currentWork)
	}
	lines = append(lines, "- Key timeline:")
	lines = append(lines, timelineBullets...)
	return strings.Join(lines, "\n")
}

func mergeCompactionSummaries(existingSummary, newSummary string) string {
	previousHighlights := extractCompactionSummaryHighlights(existingSummary)
	newHighlights := extractCompactionSummaryHighlights(newSummary)
	newTimeline := extractCompactionSummaryTimeline(newSummary)

	lines := []string{"Conversation summary:"}
	appendIndentedSummaryLines(&lines, "Previously compacted context", previousHighlights)
	appendIndentedSummaryLines(&lines, "Newly compacted context", newHighlights)
	appendIndentedSummaryLines(&lines, "Key timeline", newTimeline)
	if len(lines) == 1 {
		return newSummary
	}
	return strings.Join(lines, "\n")
}

func summarizeCompactedHistoryTerse(items []RunItem, bulletLimit int) string {
	bulletLimit = maxInt(bulletLimit, 1)
	lines := []string{
		"[COMPACTED HISTORY SUMMARY]",
		summarizeCompactionScope(items),
	}
	if tools := summarizeToolCalls(items, minInt(bulletLimit, 2)); len(tools) > 0 {
		lines = append(lines, "Tools: "+strings.Join(tools, "; ")+".")
	}
	if files := summarizeReferencedPaths(items, minInt(bulletLimit, 2)); len(files) > 0 {
		lines = append(lines, "Files: "+strings.Join(files, ", ")+".")
	}
	return strings.Join(lines, "\n")
}

func appendClawSummarySection(lines *[]string, title string, bullets []string) {
	if len(bullets) == 0 {
		return
	}
	*lines = append(*lines, "- "+title+":")
	for _, bullet := range bullets {
		bullet = strings.TrimSpace(bullet)
		if bullet == "" {
			continue
		}
		*lines = append(*lines, "  - "+bullet)
	}
}

func appendIndentedSummaryLines(lines *[]string, title string, summaries []string) {
	if len(summaries) == 0 {
		return
	}
	*lines = append(*lines, "- "+title+":")
	for _, summary := range summaries {
		summary = strings.TrimRight(summary, "\r\n")
		if strings.TrimSpace(summary) == "" {
			continue
		}
		*lines = append(*lines, "  "+summary)
	}
}

func latestExistingCompactionSummary(items []RunItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		item := items[i]
		if item.Type != RunItemMessage || item.Message == nil || item.Agent == nil || item.Agent.Name != "context-summary" {
			continue
		}
		return normalizeCompactionSummaryText(item.Message.Text)
	}
	return ""
}

func excludeCompactionSummaryItems(items []RunItem) []RunItem {
	out := make([]RunItem, 0, len(items))
	for _, item := range items {
		if item.Type == RunItemMessage && item.Message != nil && item.Agent != nil && item.Agent.Name == "context-summary" {
			continue
		}
		out = append(out, item)
	}
	return out
}

func normalizeCompactionSummaryText(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "[COMPACTED HISTORY SUMMARY]")
	text = strings.TrimSpace(text)
	if content, ok := extractTagBlock(text, "summary"); ok {
		text = "Summary:\n" + strings.TrimSpace(content)
	}
	return collapseBlankLines(text)
}

func extractCompactionSummaryHighlights(summary string) []string {
	summary = normalizeCompactionSummaryText(summary)
	var out []string
	inTimeline := false
	for _, line := range strings.Split(summary, "\n") {
		trimmedRight := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(trimmedRight)
		if trimmed == "" || trimmed == "Summary:" || trimmed == "Conversation summary:" {
			continue
		}
		if trimmed == "- Key timeline:" || trimmed == "Key timeline:" {
			inTimeline = true
			continue
		}
		if inTimeline {
			continue
		}
		out = append(out, trimmedRight)
	}
	return out
}

func extractCompactionSummaryTimeline(summary string) []string {
	summary = normalizeCompactionSummaryText(summary)
	var out []string
	inTimeline := false
	for _, line := range strings.Split(summary, "\n") {
		trimmedRight := strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(trimmedRight)
		if trimmed == "- Key timeline:" || trimmed == "Key timeline:" {
			inTimeline = true
			continue
		}
		if !inTimeline {
			continue
		}
		if trimmed == "" {
			break
		}
		out = append(out, trimmedRight)
	}
	return out
}

func extractTagBlock(content, tag string) (string, bool) {
	start := "<" + tag + ">"
	end := "</" + tag + ">"
	startIdx := strings.Index(content, start)
	if startIdx < 0 {
		return "", false
	}
	startIdx += len(start)
	endIdx := strings.Index(content[startIdx:], end)
	if endIdx < 0 {
		return "", false
	}
	return content[startIdx : startIdx+endIdx], true
}

func collapseBlankLines(content string) string {
	var lines []string
	lastBlank := false
	for _, line := range strings.Split(content, "\n") {
		blank := strings.TrimSpace(line) == ""
		if blank && lastBlank {
			continue
		}
		lines = append(lines, line)
		lastBlank = blank
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

type compactionSummaryCounts struct {
	userMessages      int
	assistantMessages int
	toolCalls         int
	toolOutputs       int
	handoffs          int
	reasoning         int
	approvals         int
}

func summarizeCompactionScope(items []RunItem) string {
	counts := countCompactedRunItems(items)
	assistantMessages := counts.assistantMessages + counts.toolCalls + counts.reasoning + counts.handoffs + counts.approvals
	toolMessages := counts.toolOutputs
	return fmt.Sprintf(
		"Scope: %d earlier messages compacted (user=%d, assistant=%d, tool=%d).",
		len(items),
		counts.userMessages,
		assistantMessages,
		toolMessages,
	)
}

func countCompactedRunItems(items []RunItem) compactionSummaryCounts {
	var counts compactionSummaryCounts
	for _, item := range items {
		switch item.Type {
		case RunItemMessage:
			if item.Message == nil {
				continue
			}
			if item.Agent == nil {
				counts.userMessages++
			} else {
				counts.assistantMessages++
			}
		case RunItemToolCall:
			if item.ToolCall != nil {
				counts.toolCalls++
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil {
				counts.toolOutputs++
			}
		case RunItemHandoffCall, RunItemHandoffOutput:
			counts.handoffs++
		case RunItemReasoning:
			counts.reasoning++
		case RunItemCompaction:
			counts.reasoning++
		case RunItemToolApproval:
			counts.approvals++
		}
	}
	return counts
}

func summarizeToolsMentioned(items []RunItem) []string {
	seen := map[string]struct{}{}
	var names []string
	for _, item := range items {
		if item.Type != RunItemToolCall || item.ToolCall == nil {
			continue
		}
		name := strings.TrimSpace(item.ToolCall.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func summarizeToolCalls(items []RunItem, limit int) []string {
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
	var pairs []pair
	for name, count := range counts {
		pairs = append(pairs, pair{name: name, count: count})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].count == pairs[j].count {
			return pairs[i].name < pairs[j].name
		}
		return pairs[i].count > pairs[j].count
	})
	out := make([]string, 0, minInt(limit, len(pairs)))
	for _, p := range pairs {
		out = append(out, fmt.Sprintf("%s ran %d time%s", p.name, p.count, pluralSuffix(p.count)))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func summarizeToolInput(input []byte) string {
	text := strings.TrimSpace(string(input))
	if text == "" || text == "{}" || text == "null" {
		return ""
	}
	text = strings.ReplaceAll(text, "\n", " ")
	return truncateLine(text, 160)
}

var summaryPathPattern = regexp.MustCompile(`(?:[A-Za-z0-9_.@+-]+/)+[A-Za-z0-9_.@+-]+\.(?:go|ts|tsx|js|jsx|json|yaml|yml|toml|md|rs|swift|proto|sql|css|scss|html|sh|py)|[A-Za-z0-9_.@+-]+\.(?:go|ts|tsx|js|jsx|json|yaml|yml|toml|md|rs|swift|proto|sql|css|scss|html|sh|py)`)

func summarizeReferencedPaths(items []RunItem, limit int) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	addMatches := func(text string) {
		if len(out) >= limit {
			return
		}
		for _, match := range summaryPathPattern.FindAllString(text, -1) {
			path := cleanSummaryPath(match)
			if path == "" || shouldSkipSummaryPath(path) {
				continue
			}
			if _, ok := seen[path]; ok {
				continue
			}
			seen[path] = struct{}{}
			out = append(out, path)
			if len(out) >= limit {
				return
			}
		}
	}
	for _, item := range items {
		switch item.Type {
		case RunItemToolCall:
			if item.ToolCall != nil {
				addMatches(string(item.ToolCall.Input))
			}
		case RunItemToolOutput:
			if item.ToolOutput != nil {
				addMatches(item.ToolOutput.Content)
			}
		case RunItemMessage:
			if item.Message != nil {
				addMatches(item.Message.Text)
			}
		}
		if len(out) >= limit {
			break
		}
	}
	return out
}

func cleanSummaryPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "`'\".,;:()[]{}<>")
	path = strings.TrimPrefix(path, "./")
	return path
}

func shouldSkipSummaryPath(path string) bool {
	if path == "" {
		return true
	}
	if strings.HasPrefix(path, ".git/") ||
		strings.HasPrefix(path, "node_modules/") ||
		strings.HasPrefix(path, "internal/dashboard/web_dist/") ||
		strings.HasPrefix(path, "web/dist/") ||
		strings.HasPrefix(path, "dist/") ||
		strings.HasPrefix(path, "build/") {
		return true
	}
	if strings.HasPrefix(path, ".") && !strings.HasPrefix(path, ".github/") {
		return true
	}
	return false
}

func inferCurrentWork(items []RunItem) string {
	for i := len(items) - 1; i >= 0; i-- {
		text := runItemTextForSummary(items[i])
		if text == "" {
			continue
		}
		return truncateLine(text, 180)
	}
	return ""
}

func summarizeKeyTimeline(items []RunItem, limit int) []string {
	if limit <= 0 || limit > len(items) {
		limit = len(items)
	}
	start := maxInt(0, len(items)-limit)
	var out []string
	for _, item := range items[start:] {
		line := summarizeRunItemForTimeline(item)
		if line == "" {
			continue
		}
		out = append(out, line)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func summarizeRunItemForTimeline(item RunItem) string {
	switch item.Type {
	case RunItemMessage:
		if item.Message == nil {
			return ""
		}
		text := strings.TrimSpace(item.Message.Text)
		if text == "" {
			return ""
		}
		if strings.HasPrefix(text, "[COMPACTED HISTORY SUMMARY]") {
			return "summary: previous compacted context"
		}
		role := "assistant"
		if item.Agent == nil {
			role = "user"
		} else if item.Agent.Name != "" {
			role = item.Agent.Name
		}
		return "  - " + role + ": " + truncateLine(text, 160)
	case RunItemToolCall:
		if item.ToolCall == nil {
			return ""
		}
		name := item.ToolCall.Name
		if strings.EqualFold(name, "bash") {
			cmd := strings.TrimSpace(ExtractBashCommand(item.ToolCall.Input))
			if cmd != "" {
				return "  - assistant: tool_use Bash(" + truncateLine(cmd, 160) + ")"
			}
		}
		if input := summarizeToolInput(item.ToolCall.Input); input != "" {
			return "  - assistant: tool_use " + name + "(" + truncateLine(input, 160) + ")"
		}
		return "  - assistant: tool_use " + name
	case RunItemToolOutput:
		if item.ToolOutput == nil {
			return ""
		}
		if item.ToolOutput.IsError {
			return "  - tool: tool_result: error " + truncateLine(item.ToolOutput.Content, 160)
		}
		return "  - tool: tool_result: " + truncateLine(item.ToolOutput.Content, 160)
	case RunItemHandoffCall:
		if item.HandoffCall != nil {
			return fmt.Sprintf("  - assistant: handoff %s -> %s", item.HandoffCall.FromAgent, item.HandoffCall.ToAgent)
		}
	case RunItemHandoffOutput:
		if item.HandoffOutput != nil {
			return fmt.Sprintf("  - tool: handoff complete %s -> %s", item.HandoffOutput.FromAgent, item.HandoffOutput.ToAgent)
		}
	case RunItemReasoning:
		if item.Reasoning != nil && strings.TrimSpace(item.Reasoning.Text) != "" {
			return "  - assistant: " + truncateLine(item.Reasoning.Text, 160)
		}
	case RunItemCompaction:
		if item.Compaction != nil {
			return "  - assistant: OpenAI compaction item"
		}
	case RunItemToolApproval:
		if item.ToolApproval != nil {
			return "  - assistant: tool_approval " + item.ToolApproval.ToolName
		}
	}
	return ""
}

func runItemTextForSummary(item RunItem) string {
	switch item.Type {
	case RunItemMessage:
		if item.Message != nil {
			return strings.TrimSpace(item.Message.Text)
		}
	case RunItemReasoning:
		if item.Reasoning != nil {
			return strings.TrimSpace(item.Reasoning.Text)
		}
	case RunItemCompaction:
		if item.Compaction != nil {
			return "OpenAI compaction item"
		}
	}
	return ""
}

func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func compactRunItemsToRecentLimit(items []RunItem, protectedPrefix []int, recentLimit, bulletLimit, targetTokens int) ([]RunItem, int, bool, string) {
	tailStart := len(items) - recentLimit
	if tailStart < 0 {
		tailStart = 0
	}

	protected := make(map[int]struct{}, len(protectedPrefix)+len(items)-tailStart)
	for _, idx := range protectedPrefix {
		protected[idx] = struct{}{}
	}
	for idx := tailStart; idx < len(items); idx++ {
		protected[idx] = struct{}{}
	}

	// Ensure tool_call/tool_output pairs are never split. If a tool_output
	// is protected its matching tool_call must be too (and vice versa),
	// otherwise the API rejects the orphaned reference.
	ensureToolPairIntegrity(items, protected)

	var removed []RunItem
	for idx, item := range items {
		if _, ok := protected[idx]; ok {
			continue
		}
		removed = append(removed, item)
	}
	if len(removed) == 0 {
		return nil, 0, false, "no-removable-history"
	}

	before := estimateRunItemsTokens(items)
	summary := summarizeCompactedHistory(removed, bulletLimit)
	if estimateStringTokens(summary)+8 >= estimateRunItemsTokens(removed) {
		summary = summarizeCompactedHistoryTerse(removed, bulletLimit)
	}
	result := buildCompactedRunItems(items, protected, summary)
	after := estimateRunItemsTokens(result)
	if targetTokens > 0 && after > targetTokens {
		summary = summarizeCompactedHistoryTerse(removed, bulletLimit)
		result = buildCompactedRunItems(items, protected, summary)
		after = estimateRunItemsTokens(result)
	}
	if targetTokens > 0 && after > targetTokens {
		summary = "[COMPACTED HISTORY SUMMARY]\nEarlier context compacted."
		result = buildCompactedRunItems(items, protected, summary)
		after = estimateRunItemsTokens(result)
	}
	if after >= before && len(result) >= len(items) {
		return nil, 0, false, "ineffective-summary"
	}
	return result, after, true, ""
}

func buildCompactedRunItems(items []RunItem, protected map[int]struct{}, summary string) []RunItem {
	result := make([]RunItem, 0, len(protected)+1)
	summaryInserted := false
	for idx := 0; idx < len(items); idx++ {
		if _, ok := protected[idx]; ok {
			result = append(result, cloneRunItem(items[idx]))
		} else if !summaryInserted {
			result = append(result, RunItem{
				Type:    RunItemMessage,
				Agent:   &Agent{Name: "context-summary"},
				Message: &MessageOutput{Text: summary},
			})
			summaryInserted = true
		}
	}
	return result
}

func collectRecentUniqueBullets(items []RunItem, limit int, fn func(RunItem) string) []string {
	if limit <= 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for i := len(items) - 1; i >= 0; i-- {
		bullet := strings.TrimSpace(fn(items[i]))
		if bullet == "" {
			continue
		}
		if _, ok := seen[bullet]; ok {
			continue
		}
		seen[bullet] = struct{}{}
		out = append(out, bullet)
		if len(out) >= limit {
			break
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func truncateLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len([]rune(s)) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max]) + "..."
}

func cloneRunItem(item RunItem) RunItem {
	clone := item
	return clone
}

// ensureToolPairIntegrity adds missing partners to the protected set so that
// every tool_output's tool_call (and every tool_call's tool_output) is also
// protected. Without this, compaction can produce orphaned tool_outputs that
// the model API rejects with "No tool call found for function call output".
func ensureToolPairIntegrity(items []RunItem, protected map[int]struct{}) {
	ensureToolPairIntegrityAfter(items, protected, 0)
}

func ensureToolPairIntegrityAfter(items []RunItem, protected map[int]struct{}, minIdx int) {
	callIDToIdx := make(map[string]int)
	outputCallIDToIdx := make(map[string]int)
	for idx, item := range items {
		if idx < minIdx {
			continue
		}
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID != "" {
			callIDToIdx[item.ToolCall.ID] = idx
		}
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID != "" {
			outputCallIDToIdx[item.ToolOutput.CallID] = idx
		}
	}

	var extras []int
	for idx := range protected {
		item := items[idx]
		if item.Type == RunItemToolOutput && item.ToolOutput != nil && item.ToolOutput.CallID != "" {
			if callIdx, ok := callIDToIdx[item.ToolOutput.CallID]; ok {
				if _, already := protected[callIdx]; !already {
					extras = append(extras, callIdx)
				}
			}
		}
		if item.Type == RunItemToolCall && item.ToolCall != nil && item.ToolCall.ID != "" {
			if outIdx, ok := outputCallIDToIdx[item.ToolCall.ID]; ok {
				if _, already := protected[outIdx]; !already {
					extras = append(extras, outIdx)
				}
			}
		}
	}
	for _, idx := range extras {
		protected[idx] = struct{}{}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
