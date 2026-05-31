package agentsdk

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNewSessionEventStreamEmitsSystemInit(t *testing.T) {
	var buf bytes.Buffer
	es := NewSessionEventStream(&buf, SessionEventStreamOptions{
		Session: 2,
		Phase:   "plan",
		SystemInit: &SessionSystemInit{
			Model:          "gpt-test",
			PermissionMode: "read-only",
			Cwd:            "/repo",
			MaxTurns:       3,
			Tools:          []string{"Read"},
			MCPServers:     []string{"docs"},
		},
	})
	es.EmitSessionEnd("completed", 0, false, 1, 2, 0, 0, 1, 10, "done")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2", len(lines))
	}
	var initEv ContentEvent
	if err := json.Unmarshal(lines[0], &initEv); err != nil {
		t.Fatal(err)
	}
	if initEv.Type != "system_init" || initEv.Session != 2 || initEv.Phase != "plan" || initEv.Model != "gpt-test" {
		t.Fatalf("system_init = %+v", initEv)
	}
	var endEv ContentEvent
	if err := json.Unmarshal(lines[1], &endEv); err != nil {
		t.Fatal(err)
	}
	if endEv.Type != "session_end" || endEv.Session != 2 || endEv.Phase != "plan" {
		t.Fatalf("session_end = %+v", endEv)
	}
}

func TestContentEventLineHelpers(t *testing.T) {
	line := `{"type":"tool_end","agent_name":"researcher","tool":"Bash","tool_use_id":"child_1","parent_call_id":"parent_1","is_error":true,"output":"exit 2\nbad","tool_duration_ms":42}`
	ev, ok := ParseContentEventLine(line)
	if !ok {
		t.Fatal("ParseContentEventLine() ok = false")
	}
	child, ok := ChildToolEventFromContentEvent(ev)
	if !ok {
		t.Fatal("ChildToolEventFromContentEvent() ok = false")
	}
	if child.ParentCallID != "parent_1" || child.CallID != "child_1" || child.Phase != "end" || child.DurationMS != 42 {
		t.Fatalf("child = %+v", child)
	}

	compact := CompactContentEvent(ev)
	for _, want := range []string{"tool_end", "agent=researcher", "tool=Bash", "error=true", "exit 2"} {
		if !strings.Contains(compact, want) {
			t.Fatalf("CompactContentEvent() = %q, missing %q", compact, want)
		}
	}
	if got := CompactContentEventLine("not-json\nsecond line"); got != "not-json" {
		t.Fatalf("CompactContentEventLine() = %q, want first raw line", got)
	}
}
