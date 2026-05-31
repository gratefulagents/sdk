package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestEventStreamEmit(t *testing.T) {
	var buf bytes.Buffer
	es := NewEventStream(&buf)
	es.SetSession(1)
	es.SetPhase("implementation")
	es.SetStep("implementing")

	es.EmitText("Hello world")
	es.EmitThinking("Let me think...")
	es.EmitToolStart("Bash", "tool-1", "parent-1", "echo hi", "planner", `{"command":"echo hi"}`)
	es.EmitToolEnd("Bash", "tool-1", "parent-1", false, "planner", "hi\n", 42)
	es.EmitCompaction(50000, 20000, "[COMPACTED HISTORY SUMMARY] earlier context")
	es.EmitSessionEnd("completed", 0.05, true, 1000, 500, 200, 100, 10, 5000, "end_turn")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 6 {
		t.Fatalf("expected 6 events, got %d:\n%s", len(lines), buf.String())
	}

	// Check first event: assistant_text
	var ev ContentEvent
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "assistant_text" {
		t.Errorf("expected assistant_text, got %s", ev.Type)
	}
	if ev.Message != "Hello world" {
		t.Errorf("expected Hello world, got %s", ev.Message)
	}
	if ev.Session != 1 {
		t.Errorf("expected session 1, got %d", ev.Session)
	}
	if ev.Phase != "implementation" {
		t.Errorf("expected phase implementation, got %q", ev.Phase)
	}
	if ev.Step != "implementing" {
		t.Errorf("expected step implementing, got %q", ev.Step)
	}

	// Check tool_start event
	var toolEv ContentEvent
	if err := json.Unmarshal([]byte(lines[2]), &toolEv); err != nil {
		t.Fatal(err)
	}
	if toolEv.Type != "tool_start" {
		t.Errorf("expected tool_start, got %s", toolEv.Type)
	}
	if toolEv.Tool != "Bash" {
		t.Errorf("expected Bash, got %s", toolEv.Tool)
	}
	if toolEv.AgentName != "planner" {
		t.Errorf("expected planner, got %s", toolEv.AgentName)
	}
	if toolEv.InputRaw != `{"command":"echo hi"}` {
		t.Errorf("expected input_raw, got %s", toolEv.InputRaw)
	}
	if toolEv.ParentCallID != "parent-1" {
		t.Errorf("expected parent-1, got %s", toolEv.ParentCallID)
	}

	// Check tool_end event
	var toolEndEv ContentEvent
	if err := json.Unmarshal([]byte(lines[3]), &toolEndEv); err != nil {
		t.Fatal(err)
	}
	if toolEndEv.Type != "tool_end" {
		t.Errorf("expected tool_end, got %s", toolEndEv.Type)
	}
	if toolEndEv.Output != "hi\n" {
		t.Errorf("expected output 'hi\\n', got %q", toolEndEv.Output)
	}
	if toolEndEv.ToolDurationMS != 42 {
		t.Errorf("expected duration 42, got %d", toolEndEv.ToolDurationMS)
	}
	if toolEndEv.ParentCallID != "parent-1" {
		t.Errorf("expected parent-1, got %s", toolEndEv.ParentCallID)
	}

	var compactEv ContentEvent
	if err := json.Unmarshal([]byte(lines[4]), &compactEv); err != nil {
		t.Fatal(err)
	}
	if compactEv.Type != "compact_boundary" {
		t.Errorf("expected compact_boundary, got %s", compactEv.Type)
	}
	if compactEv.TokensBefore != 50000 || compactEv.TokensAfter != 20000 {
		t.Errorf("compaction tokens = %d/%d, want 50000/20000", compactEv.TokensBefore, compactEv.TokensAfter)
	}
	if compactEv.Output != "[COMPACTED HISTORY SUMMARY] earlier context" {
		t.Errorf("expected compaction summary output, got %q", compactEv.Output)
	}
}

func TestChildEventStream(t *testing.T) {
	var buf bytes.Buffer
	parent := NewEventStream(&buf)
	parent.SetSession(1)
	parent.SetPhase("planning")
	parent.SetStep("exploring")

	child := NewChildEventStream(parent, "task-abc")
	child.EmitText("I'm the subagent")

	var ev ContentEvent
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.TaskID != "task-abc" {
		t.Errorf("expected task-abc, got %s", ev.TaskID)
	}
	if ev.Session != 1 {
		t.Errorf("expected session 1, got %d", ev.Session)
	}
	if ev.Phase != "planning" {
		t.Errorf("expected phase planning, got %q", ev.Phase)
	}
	if ev.Step != "exploring" {
		t.Errorf("expected step exploring, got %q", ev.Step)
	}
}

func TestEventStreamEmitLLMAttempt(t *testing.T) {
	var buf bytes.Buffer
	es := NewEventStream(&buf)
	es.SetSession(7)
	es.SetPhase("checking")
	es.SetStep("reviewing")
	es.EmitLLMAttempt(2, "subagent", "helper", "gpt-5.4", "task-123", "started")

	var ev ContentEvent
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Type != "llm_attempt" || ev.LLMAttempt != 2 || ev.LLMScope != "subagent" || ev.TaskID != "task-123" || ev.Phase != "checking" || ev.Step != "reviewing" {
		t.Fatalf("unexpected event: %+v", ev)
	}
}

func TestEventStreamCostKnownFields(t *testing.T) {
	var buf bytes.Buffer
	es := NewEventStream(&buf)
	es.EmitSubagentCompleted("task-1", "completed", "done", 2, 300, 40, 0.12, true, 3, "end_turn", "result")
	es.EmitSessionEnd("completed", 0.44, false, 10, 20, 0, 0, 1, 99, "end_turn")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 events, got %d", len(lines))
	}
	var subagentEv, sessionEv ContentEvent
	if err := json.Unmarshal([]byte(lines[0]), &subagentEv); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &sessionEv); err != nil {
		t.Fatal(err)
	}
	if !subagentEv.SubagentCostKnown {
		t.Fatalf("expected subagent cost known: %+v", subagentEv)
	}
	if sessionEv.CostKnown {
		t.Fatalf("expected session cost unknown: %+v", sessionEv)
	}
}

func TestEventStreamSubscribeReceivesChildSubagentEvents(t *testing.T) {
	var buf bytes.Buffer
	parent := NewEventStream(&buf)
	events, unsubscribe := parent.Subscribe(4)
	defer unsubscribe()

	child := NewChildEventStream(parent, "task-child")
	child.EmitSubagentTaskStatus(SubAgentTask{
		ID:               "task-child",
		AgentName:        "worker",
		Status:           SubAgentTaskWaiting,
		DependsOn:        []string{"task-parent"},
		WaitingOn:        []string{"task-parent"},
		MessagesReceived: 1,
	}, "dependency_wait")

	ev := <-events
	if ev.Type != "subagent_status" || ev.TaskID != "task-child" || ev.Status != "waiting" {
		t.Fatalf("unexpected event: %+v", ev)
	}
	subagent, ok := SubAgentStreamEventFromContentEvent(ev)
	if !ok {
		t.Fatalf("expected typed subagent event for %+v", ev)
	}
	if subagent.AgentName != "worker" || len(subagent.WaitingOn) != 1 || subagent.WaitingOn[0] != "task-parent" || subagent.MessagesReceived != 1 {
		t.Fatalf("unexpected subagent payload: %+v", subagent)
	}
}
