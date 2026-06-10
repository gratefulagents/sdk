//go:build unix

package shell

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

func newTestTerminalTool(t *testing.T) (*TerminalTool, string) {
	t.Helper()
	mgr := NewTerminalManager(sandbox.Default())
	t.Cleanup(func() { _ = mgr.Close() })
	return &TerminalTool{Manager: mgr}, t.TempDir()
}

func execTerminal(t *testing.T, tool *TerminalTool, workDir, input string) terminalSnapshot {
	t.Helper()
	res, err := tool.Execute(context.Background(), json.RawMessage(input), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", res.Content)
	}
	var snap terminalSnapshot
	if err := json.Unmarshal([]byte(res.Content), &snap); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Content, err)
	}
	return snap
}

func TestTerminalToolInteractiveSession(t *testing.T) {
	tool, workDir := newTestTerminalTool(t)

	snap := execTerminal(t, tool, workDir, `{"op":"start","wait_ms":500}`)
	if snap.SessionID == "" || snap.Status != "running" {
		t.Fatalf("unexpected start snapshot: %+v", snap)
	}
	sid := snap.SessionID

	// Run a command and observe its output.
	snap = execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"echo terminal-$((20+22))\n","wait_ms":1500}`)
	if !strings.Contains(snap.Output, "terminal-42") {
		t.Fatalf("expected command output, got: %q", snap.Output)
	}

	// Incremental read: no repeat of prior output.
	snap = execTerminal(t, tool, workDir, `{"op":"read","session_id":"`+sid+`"}`)
	if strings.Contains(snap.Output, "terminal-42") {
		t.Fatalf("read should not repeat consumed output, got: %q", snap.Output)
	}

	// Interactive program: cat reads stdin until C-d.
	execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"cat\n","wait_ms":300}`)
	snap = execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"interactive-echo\n","wait_ms":500}`)
	if !strings.Contains(snap.Output, "interactive-echo") {
		t.Fatalf("expected cat echo, got: %q", snap.Output)
	}
	execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"C-d","wait_ms":300}`)
	// Shell should be responsive again.
	snap = execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"echo back-$((1+1))\n","wait_ms":1500}`)
	if !strings.Contains(snap.Output, "back-2") {
		t.Fatalf("shell not responsive after C-d: %q", snap.Output)
	}

	// C-c interrupts a foreground process.
	execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"sleep 30\n","wait_ms":200}`)
	execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"C-c","wait_ms":300}`)
	snap = execTerminal(t, tool, workDir, `{"op":"send","session_id":"`+sid+`","keystrokes":"echo alive\n","wait_ms":1500}`)
	if !strings.Contains(snap.Output, "alive") {
		t.Fatalf("shell not responsive after C-c: %q", snap.Output)
	}

	// Kill terminates the session.
	snap = execTerminal(t, tool, workDir, `{"op":"kill","session_id":"`+sid+`"}`)
	if snap.Status != "exited" {
		t.Fatalf("expected exited after kill, got %s", snap.Status)
	}
}

func TestTerminalToolUnknownSessionAndOps(t *testing.T) {
	tool, workDir := newTestTerminalTool(t)
	res, err := tool.Execute(context.Background(), json.RawMessage(`{"op":"send","session_id":"nope","keystrokes":"x"}`), workDir)
	if err != nil || !res.IsError {
		t.Fatalf("expected error result for unknown session, got %+v err=%v", res, err)
	}
	res, err = tool.Execute(context.Background(), json.RawMessage(`{"op":"resize"}`), workDir)
	if err != nil || !res.IsError || !strings.Contains(res.Content, "invalid op") {
		t.Fatalf("expected invalid op error, got %+v err=%v", res, err)
	}
}

func TestLimitTerminalOutputHeadTail(t *testing.T) {
	long := strings.Repeat("h", 8000) + "MID" + strings.Repeat("t", 8000)
	out := limitTerminalOutput(long, 10000)
	if len(out) > 11000 {
		t.Fatalf("output too long: %d", len(out))
	}
	if !strings.HasPrefix(out, "hhh") || !strings.HasSuffix(out, "ttt") {
		t.Fatal("head/tail not preserved")
	}
	if !strings.Contains(out, "interior bytes omitted") {
		t.Fatal("missing omission marker")
	}
	if got := limitTerminalOutput("short", 10000); got != "short" {
		t.Fatalf("short output should be unchanged: %q", got)
	}
}

func TestTerminalScrollbackWindowDiscardsOldOutput(t *testing.T) {
	s := &terminalSession{done: make(chan struct{}), startedAt: time.Now()}
	// Simulate the read loop sliding the window before any read happened:
	// the unread cursor (0) falls behind the window start.
	s.mu.Lock()
	s.stream = make([]byte, terminalWindowBytes)
	s.streamStart = 1000
	s.mu.Unlock()
	_, dropped := s.consumeNewOutput()
	if !dropped {
		t.Fatal("expected dropped flag when read offset fell behind window start")
	}
	// Cursor is caught up now; next read reports nothing dropped.
	if _, dropped := s.consumeNewOutput(); dropped {
		t.Fatal("dropped flag should reset once the cursor caught up")
	}
}

func TestBashPollIncrementalOutput(t *testing.T) {
	mgr := NewAsyncManager(sandbox.Default())
	t.Cleanup(func() { _ = mgr.Close() })
	start := &BashStartTool{Manager: mgr}
	poll := &BashPollTool{Manager: mgr}
	workDir := t.TempDir()

	res, err := start.Execute(context.Background(), json.RawMessage(`{"command":"echo first; sleep 1; echo second"}`), workDir)
	if err != nil || res.IsError {
		t.Fatalf("start failed: %+v err=%v", res, err)
	}
	id := strings.TrimPrefix(res.Content, "started background bash job ")

	// First poll sees "first".
	res, err = poll.Execute(context.Background(), json.RawMessage(`{"id":"`+id+`","wait_ms":500}`), workDir)
	if err != nil || res.IsError {
		t.Fatalf("poll failed: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Content, "first") {
		t.Fatalf("first poll missing output: %s", res.Content)
	}

	// Second poll (after completion) sees only "second", not "first".
	res, err = poll.Execute(context.Background(), json.RawMessage(`{"id":"`+id+`","wait_ms":3000}`), workDir)
	if err != nil || res.IsError {
		t.Fatalf("poll failed: %+v err=%v", res, err)
	}
	if strings.Contains(res.Content, "first") {
		t.Fatalf("incremental poll repeated old output: %s", res.Content)
	}
	if !strings.Contains(res.Content, "second") {
		t.Fatalf("incremental poll missing new output: %s", res.Content)
	}

	// Full output still available with incremental=false.
	res, err = poll.Execute(context.Background(), json.RawMessage(`{"id":"`+id+`","incremental":false}`), workDir)
	if err != nil || res.IsError {
		t.Fatalf("poll failed: %+v err=%v", res, err)
	}
	if !strings.Contains(res.Content, "first") || !strings.Contains(res.Content, "second") {
		t.Fatalf("full poll missing output: %s", res.Content)
	}
}
