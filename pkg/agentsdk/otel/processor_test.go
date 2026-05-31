package otel

import (
	"testing"

	agent "github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestMapSpanData_Generation(t *testing.T) {
	s := agent.NewSpan("gen", "", agent.GenerationSpanData{
		RequestedModel:    "openai/gpt-4",
		ResolvedModel:     "gpt-4",
		ModelProvider:     "openai",
		ModelCanonical:    "gpt-4",
		AttemptNumber:     2,
		Scope:             "subagent",
		TaskID:            "task-1",
		Phase:             "planning",
		UsageAvailable:    true,
		PromptTokens:      100,
		CompletionTokens:  50,
		CacheReadTokens:   10,
		CacheCreateTokens: 5,
		TotalTokens:       150,
		CostUSD:           0.0123,
		CostKnown:         true,
		Success:           true,
	})
	name, attrs := mapSpanData(s)
	if name != "llm.generation" {
		t.Errorf("expected llm.generation, got %s", name)
	}
	if len(attrs) != 30 {
		t.Errorf("expected 30 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Function(t *testing.T) {
	s := agent.NewSpan("fn", "", agent.FunctionSpanData{
		ToolName: "bash",
		Input:    "echo hello",
		Output:   "hello",
		IsError:  false,
	})
	name, attrs := mapSpanData(s)
	if name != "tool.bash" {
		t.Errorf("expected tool.bash, got %s", name)
	}
	if len(attrs) != 4 {
		t.Errorf("expected 4 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Handoff(t *testing.T) {
	s := agent.NewSpan("ho", "", agent.HandoffSpanData{FromAgent: "planner", ToAgent: "executor"})
	name, attrs := mapSpanData(s)
	if name != "handoff" {
		t.Errorf("expected handoff, got %s", name)
	}
	if len(attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Agent(t *testing.T) {
	s := agent.NewSpan("agent", "", agent.AgentSpanData{AgentName: "planner", Instructions: "Plan the task"})
	name, attrs := mapSpanData(s)
	if name != "agent.planner" {
		t.Errorf("expected agent.planner, got %s", name)
	}
	if len(attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Guardrail(t *testing.T) {
	s := agent.NewSpan("gr", "", agent.GuardrailSpanData{GuardrailName: "secret-check", Triggered: true})
	name, attrs := mapSpanData(s)
	if name != "guardrail.secret-check" {
		t.Errorf("expected guardrail.secret-check, got %s", name)
	}
	if len(attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Session(t *testing.T) {
	s := agent.NewSpan("session", "", agent.SessionSpanData{
		Model:        "gpt-4",
		CostUSD:      0.05,
		NumTurns:     10,
		DurationMS:   5000,
		InputTokens:  1000,
		OutputTokens: 500,
		StopReason:   "end_turn",
	})
	name, attrs := mapSpanData(s)
	if name != "session" {
		t.Errorf("expected session, got %s", name)
	}
	if len(attrs) != 9 {
		t.Errorf("expected 9 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Subagent(t *testing.T) {
	s := agent.NewSpan("subagent", "", agent.SubagentSpanData{
		TaskID: "t1", Type: "executor", Description: "Build it",
		Model: "gpt-4", Status: "completed", CostUSD: 0.02,
	})
	name, attrs := mapSpanData(s)
	if name != "subagent.executor" {
		t.Errorf("expected subagent.executor, got %s", name)
	}
	if len(attrs) != 13 {
		t.Errorf("expected 13 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Retry(t *testing.T) {
	s := agent.NewSpan("retry", "", agent.RetrySpanData{
		ErrorCode: "rate_limit", RetryAfterMS: 1000, Attempt: 2, MaxRetries: 5,
	})
	name, attrs := mapSpanData(s)
	if name != "api.retry" {
		t.Errorf("expected api.retry, got %s", name)
	}
	if len(attrs) != 4 {
		t.Errorf("expected 4 attrs, got %d", len(attrs))
	}
}

func TestMapSpanData_Compaction(t *testing.T) {
	s := agent.NewSpan("compact", "", agent.CompactionSpanData{TokensBefore: 50000, TokensAfter: 20000})
	name, attrs := mapSpanData(s)
	if name != "compaction" {
		t.Errorf("expected compaction, got %s", name)
	}
	if len(attrs) != 2 {
		t.Errorf("expected 2 attrs, got %d", len(attrs))
	}
}

func TestOTelTruncate(t *testing.T) {
	short := "hello"
	if got := truncate(short, 10); got != "hello" {
		t.Errorf("expected hello, got %s", got)
	}
	long := "hello world this is a long string"
	got := truncate(long, 10)
	if got != "hello worl..." {
		t.Errorf("expected 'hello worl...', got %s", got)
	}
}
