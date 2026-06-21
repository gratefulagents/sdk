package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestCollectSubagentResultBlocksUntilTerminal verifies that
// collect_subagent_result waits for an in-flight task instead of returning a
// "still running" error (which forces the parent to busy-poll and burn turns).
func TestCollectSubagentResultBlocksUntilTerminal(t *testing.T) {
	model := &blockingSubagentToolModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	spawn := &spawnSubagentTaskTool{registry: registry, defaultAgent: "worker"}
	collect := &collectSubagentResultTool{registry: registry}

	res, err := spawn.Execute(context.Background(), json.RawMessage(`{"message":"do it"}`), "")
	if err != nil || res.IsError {
		t.Fatalf("spawn failed: err=%v content=%s", err, res.Content)
	}
	var spawned struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(res.Content), &spawned); err != nil {
		t.Fatal(err)
	}

	// Wait until the child's model call is in-flight (task not yet terminal).
	<-model.started

	type collectOut struct {
		result ToolResult
		err    error
	}
	done := make(chan collectOut, 1)
	go func() {
		r, e := collect.Execute(context.Background(), json.RawMessage(`{"task_id":"`+spawned.TaskID+`"}`), "")
		done <- collectOut{r, e}
	}()

	// collect must still be blocking while the child runs.
	select {
	case <-done:
		t.Fatal("collect_subagent_result returned before the task was terminal")
	case <-time.After(100 * time.Millisecond):
	}

	close(model.release)

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatal(out.err)
		}
		if out.result.IsError {
			t.Fatalf("collect returned error: %s", out.result.Content)
		}
		if !strings.Contains(out.result.Content, "blocked child done") {
			t.Fatalf("collect content = %q, want the child result", out.result.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("collect_subagent_result did not return after the task finished")
	}
}

// TestCollectSubagentResultRespectsTimeout verifies that a bounded wait returns
// an error (rather than hanging) when the task does not finish in time.
func TestCollectSubagentResultRespectsTimeout(t *testing.T) {
	model := &blockingSubagentToolModel{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	registry := NewSubAgentScheduler(SubAgentSchedulerConfig{
		Runner: NewRunnerWithModel(model),
		Agents: map[string]*Agent{"worker": {Name: "worker"}},
	})
	spawn := &spawnSubagentTaskTool{registry: registry, defaultAgent: "worker"}
	collect := &collectSubagentResultTool{registry: registry}

	res, err := spawn.Execute(context.Background(), json.RawMessage(`{"message":"x"}`), "")
	if err != nil || res.IsError {
		t.Fatalf("spawn failed: err=%v content=%s", err, res.Content)
	}
	var spawned struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(res.Content), &spawned); err != nil {
		t.Fatal(err)
	}
	<-model.started

	out, err := collect.Execute(context.Background(), json.RawMessage(`{"task_id":"`+spawned.TaskID+`","timeout_ms":50}`), "")
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError {
		t.Fatalf("expected a timeout error, got %q", out.Content)
	}
	close(model.release) // let the child finish so the goroutine exits cleanly
}
