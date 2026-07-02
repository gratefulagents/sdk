package agent

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	// llmSummaryMaxOutputTokens bounds the summary generation. ~2k tokens is
	// enough for a dense continuation brief without eating the freed budget.
	llmSummaryMaxOutputTokens = 2048
	// llmSummaryTranscriptCharBudget caps the flattened transcript sent to the
	// summarizer (~60k tokens) so the summary call itself fits comfortably in
	// any realistic model window, including after a context-overflow error.
	llmSummaryTranscriptCharBudget = 240_000
	// llmSummaryCallTimeout bounds the summary call when no run-level model
	// call timeout is configured.
	llmSummaryCallTimeout = 120 * time.Second
)

// llmSummaryInstructions is the system prompt for LLM compaction summaries.
// The goal is a continuation brief, not a recap: the agent must be able to
// resume mid-task without re-reading the dropped history.
const llmSummaryInstructions = `You are compacting the working history of an AI coding agent mid-task. The transcript segment below is about to be deleted from the agent's context; your summary is the ONLY memory of it the agent will keep. Write a dense continuation brief so the agent can resume seamlessly.

Preserve, in this order:
1. Task & intent: what the user asked for, including exact constraints and any follow-up corrections.
2. Findings: what was learned about the codebase/system (key files, symbols, behaviors, root causes), with concrete paths and line references where present.
3. Decisions: design/implementation decisions made and the reasons behind them.
4. Actions & state: files created/edited (paths + what changed), commands run and their outcomes, tests passing/failing, commits made.
5. In-progress work: exactly what was being done last and the planned next steps.
6. Pitfalls: errors hit, dead ends explored, approaches ruled out (so they are not retried).

Rules:
- Be specific: real paths, symbol names, numbers, error strings. Never write vague fillers like "several files were inspected".
- Use terse bullets grouped under the headings above; omit headings with nothing to say.
- Do not mention the summarization process itself. Output only the brief.`

// summarizeRemovedItemsWithModel asks the model to summarize the items being
// compacted away. Returns the summary body text (no compaction marker).
func summarizeRemovedItemsWithModel(ctx context.Context, model Model, modelName string, removed []RunItem, timeout time.Duration) (string, Usage, error) {
	if model == nil {
		return "", Usage{}, fmt.Errorf("no model available for compaction summary")
	}
	transcript := flattenRunItemsForSummary(removed, llmSummaryTranscriptCharBudget)
	if strings.TrimSpace(transcript) == "" {
		return "", Usage{}, fmt.Errorf("empty transcript for compaction summary")
	}
	if timeout <= 0 {
		timeout = llmSummaryCallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req := ModelRequest{
		Model:        modelName,
		Instructions: llmSummaryInstructions,
		Input: []RunItem{{
			Type:    RunItemMessage,
			Message: &MessageOutput{Text: "Transcript segment to summarize:\n\n" + transcript},
		}},
		Settings: ModelSettings{
			MaxTokens:       llmSummaryMaxOutputTokens,
			ReasoningEffort: "low",
		},
	}
	resp, err := model.GetResponse(callCtx, req)
	if err != nil {
		return "", Usage{}, err
	}
	var parts []string
	for _, item := range resp.Items {
		if item.Type == RunItemMessage && item.Message != nil && strings.TrimSpace(item.Message.Text) != "" {
			parts = append(parts, strings.TrimSpace(item.Message.Text))
		}
	}
	body := strings.TrimSpace(strings.Join(parts, "\n"))
	body = strings.TrimSpace(strings.TrimPrefix(body, "[COMPACTED HISTORY SUMMARY]"))
	if body == "" {
		return "", resp.Usage, fmt.Errorf("model returned empty compaction summary")
	}
	return body, resp.Usage, nil
}

// applyLLMSummaryToPlan swaps the plan's deterministic summary for an
// LLM-written one when that still shrinks the history. Returns the rebuilt
// items and true on success; callers keep the deterministic plan on false.
func applyLLMSummaryToPlan(ctx context.Context, model Model, modelName string, plan CompactionPlan, timeout time.Duration) ([]RunItem, Usage, bool) {
	body, usage, err := summarizeRemovedItemsWithModel(ctx, model, modelName, plan.Removed, timeout)
	if err != nil {
		log.Printf("[runner] WARN: LLM compaction summary failed; keeping deterministic summary: %v", err)
		return nil, usage, false
	}
	rebuilt := plan.RebuildWithSummary(body)
	// The LLM body is output-capped, so this only rejects pathological cases
	// (e.g. tiny histories where any prose summary is a net loss).
	if before := estimateRunItemsTokens(plan.Source); estimateRunItemsTokens(rebuilt) >= before {
		log.Printf("[runner] WARN: LLM compaction summary did not shrink history; keeping deterministic summary")
		return nil, usage, false
	}
	return rebuilt, usage, true
}

// flattenRunItemsForSummary renders run items as a role-labeled plain-text
// transcript for the summarizer. Long payloads are truncated per item and the
// whole transcript is middle-out truncated to maxChars, keeping the head
// (original task) and tail (most recent state).
func flattenRunItemsForSummary(items []RunItem, maxChars int) string {
	var b strings.Builder
	for _, item := range items {
		switch item.Type {
		case RunItemMessage:
			if item.Message == nil || strings.TrimSpace(item.Message.Text) == "" {
				continue
			}
			role := "assistant"
			if item.Agent == nil {
				role = "user"
			}
			fmt.Fprintf(&b, "[%s] %s\n\n", role, truncateForTranscript(item.Message.Text, 2000))
		case RunItemReasoning:
			if item.Reasoning == nil || strings.TrimSpace(item.Reasoning.Text) == "" {
				continue
			}
			fmt.Fprintf(&b, "[thinking] %s\n\n", truncateForTranscript(item.Reasoning.Text, 1500))
		case RunItemToolCall:
			if item.ToolCall == nil {
				continue
			}
			fmt.Fprintf(&b, "[tool_call] %s %s\n", item.ToolCall.Name, truncateForTranscript(string(item.ToolCall.Input), 300))
		case RunItemToolOutput:
			if item.ToolOutput == nil {
				continue
			}
			limit := 700
			label := "tool_result"
			if item.ToolOutput.IsError {
				limit = 1000
				label = "tool_error"
			}
			fmt.Fprintf(&b, "[%s] %s\n\n", label, truncateForTranscript(item.ToolOutput.Content, limit))
		}
	}
	transcript := b.String()
	if maxChars > 0 && len(transcript) > maxChars {
		head := maxChars / 3
		tail := maxChars - head
		transcript = transcript[:head] + "\n\n[... transcript truncated ...]\n\n" + transcript[len(transcript)-tail:]
	}
	return transcript
}

func truncateForTranscript(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + " …[truncated]"
}
