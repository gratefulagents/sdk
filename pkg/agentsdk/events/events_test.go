package events

import (
	"strings"
	"testing"
)

func TestLineWriterEmitsChildToolEvents(t *testing.T) {
	var got []Event
	writer := NewLineWriter(SinkFunc(func(ev Event) {
		got = append(got, ev)
	}))

	_, err := writer.Write([]byte(`{"type":"tool_end","agent_name":"researcher","tool":"Bash","tool_use_id":"child_1","parent_call_id":"parent_1","is_error":true,"output":"exit 2","tool_duration_ms":42}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Kind != KindChildTool || got[0].Child == nil {
		t.Fatalf("event = %+v, want child tool event", got[0])
	}
	if got[0].Child.ParentCallID != "parent_1" || got[0].Child.DurationMS != 42 {
		t.Fatalf("child = %+v", got[0].Child)
	}
}

func TestLineWriterEmitsSubAgentEvents(t *testing.T) {
	var got []Event
	writer := NewLineWriter(SinkFunc(func(ev Event) {
		got = append(got, ev)
	}))

	_, err := writer.Write([]byte(`{"type":"subagent_status","task_id":"task_1","agent_name":"researcher","status":"running","message":"running","subagent_waiting_on":["task_0"],"subagent_messages_received":2}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	if got[0].Kind != KindSubAgent || got[0].SubAgent == nil {
		t.Fatalf("event = %+v, want subagent event", got[0])
	}
	if got[0].SubAgent.TaskID != "task_1" || got[0].SubAgent.AgentName != "researcher" || got[0].SubAgent.Status != "running" || got[0].SubAgent.MessagesReceived != 2 {
		t.Fatalf("subagent = %+v", got[0].SubAgent)
	}
	if len(got[0].SubAgent.WaitingOn) != 1 || got[0].SubAgent.WaitingOn[0] != "task_0" {
		t.Fatalf("waiting_on = %+v", got[0].SubAgent.WaitingOn)
	}
}

func TestLineWriterBuffersPartialLines(t *testing.T) {
	var got []Event
	writer := NewLineWriter(SinkFunc(func(ev Event) {
		got = append(got, ev)
	}))

	if _, err := writer.Write([]byte(`{"type":"tool_end","tool":"Bash","is_error":true,"output":"hello`)); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("events after partial write = %d, want 0", len(got))
	}
	if _, err := writer.Write([]byte(` world"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != KindLog || !strings.Contains(got[0].Text, "hello world") {
		t.Fatalf("event = %+v, want compact log with combined output", got)
	}
}
