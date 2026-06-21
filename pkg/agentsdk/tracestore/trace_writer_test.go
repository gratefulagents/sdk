package tracestore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestAddSpanDataGenerationUsesCurrentFields(t *testing.T) {
	tw := &TraceWriter{}
	entry := map[string]any{}

	tw.addSpanData(entry, &agent.GenerationSpanData{
		RequestedModel:     "openai/gpt-5.4",
		ResolvedModel:      "gpt-5.4",
		ModelProvider:      "openai",
		ModelCanonical:     "openai/gpt-5.4",
		AttemptNumber:      2,
		Turn:               3,
		Scope:              "top_level",
		TaskID:             "task-123",
		Phase:              "analysis",
		Status:             "completed",
		UsageAvailable:     true,
		PromptTokens:       101,
		CompletionTokens:   42,
		TotalTokens:        143,
		LatencyMS:          987,
		Success:            true,
		RetryScheduled:     false,
		FallbackScheduled:  true,
		FallbackFromModel:  "openai/gpt-5.4",
		FallbackToModel:    "anthropic/claude-sonnet-4-6",
		FallbackReason:     "rate_limit",
		ToolCount:          4,
		InputItemCount:     2,
		OutputItemCount:    1,
		InstructionsLength: 321,
		Request: &agent.LLMRequestSnapshot{
			AgentName:    "planner",
			Model:        "gpt-5.4",
			Instructions: "resolved instructions",
		},
		Response: &agent.LLMResponseSnapshot{
			Texts:          []string{"answer"},
			ReasoningTexts: []string{"thinking"},
			ThinkingTexts:  []string{"thinking"},
		},
	})

	if got := entry["span_type"]; got != "generation" {
		t.Fatalf("span_type = %v, want generation", got)
	}
	if got := entry["model"]; got != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", got)
	}
	if got := entry["fallback_to_model"]; got != "anthropic/claude-sonnet-4-6" {
		t.Fatalf("fallback_to_model = %v", got)
	}
	if got := entry["input_tokens"]; got != int64(101) {
		t.Fatalf("input_tokens = %v, want 101", got)
	}
	if got := entry["output_tokens"]; got != int64(42) {
		t.Fatalf("output_tokens = %v, want 42", got)
	}
	if got := entry["gen_duration_ms"]; got != int64(987) {
		t.Fatalf("gen_duration_ms = %v, want 987", got)
	}
	if got := entry["resolved_model"]; got != "gpt-5.4" {
		t.Fatalf("resolved_model = %v, want gpt-5.4", got)
	}
	if got := entry["phase"]; got != "analysis" {
		t.Fatalf("phase = %v, want analysis", got)
	}
	if got := entry["request"]; got == nil {
		t.Fatal("request snapshot missing")
	}
	if got := entry["response"]; got == nil {
		t.Fatal("response snapshot missing")
	}
}

func TestAddSpanDataSessionHandlesValueSpanData(t *testing.T) {
	tw := &TraceWriter{}
	entry := map[string]any{}

	tw.addSpanData(entry, agent.SessionSpanData{
		Model:                    "gpt-5.4",
		CostUSD:                  1.25,
		NumTurns:                 4,
		DurationMS:               1200,
		InputTokens:              500,
		OutputTokens:             250,
		CacheReadInputTokens:     50,
		CacheCreationInputTokens: 25,
		StopReason:               "completed",
	})

	if got := entry["span_type"]; got != "session" {
		t.Fatalf("span_type = %v, want session", got)
	}
	if got := entry["model"]; got != "gpt-5.4" {
		t.Fatalf("model = %v, want gpt-5.4", got)
	}
	if got := entry["input_tokens"]; got != int64(500) {
		t.Fatalf("input_tokens = %v, want 500", got)
	}
	if got := entry["duration_ms"]; got != int64(1200) {
		t.Fatalf("duration_ms = %v, want 1200", got)
	}
}

func TestTraceWriterMirrorsGenerationSnapshotsToLLMCalls(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFilesystemTraceStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	tw := NewTraceWriter(store)
	if err := tw.InitRun(RunMetadata{RunID: "run-1", StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	span := agent.NewSpan("generation", "", &agent.GenerationSpanData{
		RequestedModel: "openai/gpt-5.5",
		ResolvedModel:  "gpt-5.5",
		AttemptNumber:  1,
		Turn:           2,
		Status:         "started",
		Request: &agent.LLMRequestSnapshot{
			AgentName:    "engineer",
			Model:        "gpt-5.5",
			Instructions: "resolved instructions",
			InputItems: []agent.LLMRunItemSnapshot{
				{Type: "message", MessageText: "hello"},
			},
		},
	})
	tw.OnSpanStart(span)

	data := span.Data.(*agent.GenerationSpanData)
	data.Status = "completed"
	data.Success = true
	data.Response = &agent.LLMResponseSnapshot{
		Texts:          []string{"done"},
		ReasoningTexts: []string{"provider-visible thinking"},
		ThinkingTexts:  []string{"provider-visible thinking"},
	}
	span.Finish()
	tw.OnSpanEnd(span)

	llmBytes, err := os.ReadFile(filepath.Join(dir, "traces", "run-1", "llm_calls.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(llmBytes)), "\n")
	if len(lines) != 2 {
		t.Fatalf("llm_calls lines = %d, want 2: %s", len(lines), llmBytes)
	}

	var startEntry, endEntry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &startEntry); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &endEntry); err != nil {
		t.Fatal(err)
	}
	if startEntry["type"] != "generation_start" {
		t.Fatalf("start type = %v", startEntry["type"])
	}
	if endEntry["type"] != "generation_end" {
		t.Fatalf("end type = %v", endEntry["type"])
	}
	req := startEntry["request"].(map[string]any)
	if req["instructions"] != "resolved instructions" {
		t.Fatalf("request instructions = %v", req["instructions"])
	}
	resp := endEntry["response"].(map[string]any)
	reasoning := resp["reasoning_texts"].([]any)
	if len(reasoning) != 1 || reasoning[0] != "provider-visible thinking" {
		t.Fatalf("reasoning_texts = %+v", reasoning)
	}

	resolvedPath := filepath.Join(dir, "traces", "run-1", "resolved_instructions", "turn_002_attempt_001.txt")
	resolved, err := os.ReadFile(resolvedPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved) != "resolved instructions" {
		t.Fatalf("resolved instructions file = %q", resolved)
	}
}
