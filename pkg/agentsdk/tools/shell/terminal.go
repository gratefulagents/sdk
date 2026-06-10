package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

const (
	// terminalMaxReturnBytes caps the output returned to the model per
	// Terminal call, keeping the head and tail (Terminus 2 pattern).
	terminalMaxReturnBytes = 10_000
	// terminalWindowBytes is the sliding scrollback window retained per session.
	terminalWindowBytes = 256 * 1024
	// terminalMaxWait caps the post-keystroke wait. Terminus guidance: never
	// wait longer than 60 seconds; poll again instead.
	terminalMaxWait = 60 * time.Second
	// terminalDefaultWait is applied when the model omits wait_ms.
	terminalDefaultWait = 1 * time.Second
	// terminalRows/Cols match the Terminal-Bench reference pane size.
	terminalRows = 40
	terminalCols = 160
)

// TerminalManager owns interactive PTY sessions started by the Terminal tool.
type TerminalManager struct {
	mu       sync.Mutex
	nextID   int
	executor sandbox.Executor
	sessions map[string]*terminalSession
}

// NewTerminalManager creates a manager for interactive terminal sessions.
func NewTerminalManager(executor sandbox.Executor) *TerminalManager {
	return &TerminalManager{executor: executor, sessions: map[string]*terminalSession{}}
}

// Close terminates all sessions.
func (m *TerminalManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	sessions := make([]*terminalSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.mu.Unlock()
	for _, s := range sessions {
		s.kill()
	}
	return nil
}

type terminalSession struct {
	id        string
	cmd       *exec.Cmd
	ptyFile   *os.File
	startedAt time.Time
	done      chan struct{}

	mu          sync.Mutex
	stream      []byte // sliding scrollback window
	streamStart int64  // absolute offset of stream[0]
	readOffset  int64  // absolute offset of the next unread byte
	exited      bool
	exitErr     error
	endedAt     time.Time
}

func (m *TerminalManager) start(ctx context.Context, workDir string, mode policy.PermissionMode) (*terminalSession, error) {
	if m == nil {
		return nil, errors.New("terminal manager is nil")
	}
	executor := m.executor
	if executor == nil {
		executor = sandbox.Default()
	}
	req := sandbox.Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-i"},
		WorkDir:        workDir,
		PermissionMode: mode,
	}
	built, err := executor.Build(ctx, req)
	if err != nil {
		return nil, err
	}
	args := append([]string(nil), built.Args[1:]...)
	cmd := exec.Command(built.Path, args...)
	cmd.Dir = built.Dir
	cmd.Env = append(append([]string(nil), built.Env...), "TERM=xterm-256color")

	ptyFile, err := startTerminalPTY(cmd, terminalRows, terminalCols)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.nextID++
	id := "term-" + strconv.Itoa(m.nextID)
	s := &terminalSession{
		id:        id,
		cmd:       cmd,
		ptyFile:   ptyFile,
		startedAt: time.Now(),
		done:      make(chan struct{}),
	}
	m.sessions[id] = s
	m.mu.Unlock()

	go s.readLoop()
	go s.waitLoop()
	return s, nil
}

func (m *TerminalManager) get(id string) (*terminalSession, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (m *TerminalManager) list() []*terminalSession {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*terminalSession, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	return out
}

// readLoop drains PTY output into the sliding scrollback window.
func (s *terminalSession) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := s.ptyFile.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.stream = append(s.stream, buf[:n]...)
			if overflow := len(s.stream) - terminalWindowBytes; overflow > 0 {
				s.stream = append([]byte(nil), s.stream[overflow:]...)
				s.streamStart += int64(overflow)
			}
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *terminalSession) waitLoop() {
	err := s.cmd.Wait()
	s.mu.Lock()
	s.exited = true
	s.exitErr = err
	s.endedAt = time.Now()
	s.mu.Unlock()
	_ = s.ptyFile.Close()
	close(s.done)
}

func (s *terminalSession) kill() {
	if s == nil {
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	killProcessTree(s.cmd)
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

// send writes keystrokes to the PTY. Whole-string special tokens are
// translated to control bytes; everything else is sent verbatim.
func (s *terminalSession) send(keystrokes string) error {
	select {
	case <-s.done:
		return errors.New("session has exited")
	default:
	}
	_, err := s.ptyFile.Write(translateKeystrokes(keystrokes))
	return err
}

// consumeNewOutput returns output produced since the previous read and
// advances the read cursor.
func (s *terminalSession) consumeNewOutput() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	streamEnd := s.streamStart + int64(len(s.stream))
	dropped := false
	from := s.readOffset
	if from < s.streamStart {
		dropped = true
		from = s.streamStart
	}
	if from >= streamEnd {
		s.readOffset = streamEnd
		return "", dropped
	}
	out := string(s.stream[from-s.streamStart:])
	s.readOffset = streamEnd
	return out, dropped
}

// translateKeystrokes maps standalone special-key tokens (tmux-style) to raw
// control bytes. Mixed strings are sent verbatim, which is why the tool
// instructs the model to send C-c/C-d as standalone calls.
func translateKeystrokes(s string) []byte {
	switch s {
	case "C-c":
		return []byte{0x03}
	case "C-d":
		return []byte{0x04}
	case "C-z":
		return []byte{0x1a}
	case "C-l":
		return []byte{0x0c}
	case "Enter":
		return []byte{'\r'}
	case "Escape":
		return []byte{0x1b}
	case "Tab":
		return []byte{'\t'}
	case "Up":
		return []byte{0x1b, '[', 'A'}
	case "Down":
		return []byte{0x1b, '[', 'B'}
	case "Right":
		return []byte{0x1b, '[', 'C'}
	case "Left":
		return []byte{0x1b, '[', 'D'}
	}
	return []byte(s)
}

// limitTerminalOutput keeps the head and tail of oversized output so both the
// command echo and the final prompt/result survive truncation.
func limitTerminalOutput(out string, maxBytes int) string {
	if maxBytes <= 0 || len(out) <= maxBytes {
		return out
	}
	half := maxBytes / 2
	omitted := len(out) - 2*half
	return out[:half] +
		fmt.Sprintf("\n[... output limited to %d bytes; %d interior bytes omitted ...]\n", maxBytes, omitted) +
		out[len(out)-half:]
}

type terminalInput struct {
	Op         string `json:"op"`
	SessionID  string `json:"session_id"`
	Keystrokes string `json:"keystrokes"`
	WaitMS     int    `json:"wait_ms"`
}

type terminalSnapshot struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Output    string `json:"output,omitempty"`
	Note      string `json:"note,omitempty"`
}

func (s *terminalSession) snapshot(output string, dropped bool) terminalSnapshot {
	s.mu.Lock()
	exited := s.exited
	endedAt := s.endedAt
	s.mu.Unlock()
	status := "running"
	if exited {
		status = "exited"
	}
	elapsed := time.Since(s.startedAt)
	if !endedAt.IsZero() {
		elapsed = endedAt.Sub(s.startedAt)
	}
	snap := terminalSnapshot{
		SessionID: s.id,
		Status:    status,
		ElapsedMS: elapsed.Milliseconds(),
		Output:    limitTerminalOutput(output, terminalMaxReturnBytes),
	}
	if dropped {
		snap.Note = "older scrollback before this output was discarded"
	}
	if output == "" {
		snap.Note = strings.TrimSpace(snap.Note + " no new output since last read; wait and read again if a command is still running")
	}
	return snap
}

// TerminalTool drives a persistent interactive PTY session. It exists for
// workflows one-shot Bash cannot handle: TUIs (vim, less, htop), debuggers,
// REPLs, ssh prompts, and long-lived foreground processes.
type TerminalTool struct {
	Manager *TerminalManager
	Mode    policy.PermissionMode
}

func (t *TerminalTool) Name() string { return "Terminal" }

func (t *TerminalTool) Description() string {
	return "Interact with a persistent terminal (PTY) session. Use for interactive programs (vim, gdb, REPLs, ssh, installers with prompts) and long-running foreground processes; use Bash for ordinary one-shot commands. Keystrokes are sent verbatim: end shell commands with \\n to execute them. Send special keys (C-c, C-d, Escape, Enter, arrow keys) as standalone calls with only that token in keystrokes. Prefer short waits and repeated reads over one long wait."
}

func (t *TerminalTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"op": {
				"type": "string",
				"enum": ["start", "send", "read", "kill", "list"],
				"description": "start: open a new session (returns session_id). send: write keystrokes, wait wait_ms, return new output. read: return output produced since the last read. kill: terminate the session. list: list sessions."
			},
			"session_id": {"type": "string", "description": "Session id returned by start. Required for send/read/kill."},
			"keystrokes": {"type": "string", "description": "Raw keystrokes for send. End commands with \\n to execute. Special keys (C-c, C-d, C-z, Escape, Enter, Tab, Up/Down/Left/Right) must be the entire string."},
			"wait_ms": {"type": "number", "description": "How long to wait after sending before capturing output (default 1000, max 60000). Use small values for instant commands and poll with read for slow ones."}
		},
		"required": ["op"]
	}`)
}

func (t *TerminalTool) IsReadOnly() bool { return false }
func (t *TerminalTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}
func (t *TerminalTool) NeedsApproval() bool { return false }
func (t *TerminalTool) TimeoutSeconds() int { return 0 }

func (t *TerminalTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in terminalInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	switch strings.TrimSpace(in.Op) {
	case "start":
		return t.executeStart(ctx, in, workDir)
	case "send":
		return t.executeSend(ctx, in)
	case "read":
		return t.executeRead(ctx, in)
	case "kill":
		return t.executeKill(in)
	case "list":
		return t.executeList()
	default:
		return agentsdk.ToolResult{Content: fmt.Sprintf("invalid op %q: use start, send, read, kill, or list", in.Op), IsError: true}, nil
	}
}

func (t *TerminalTool) executeStart(ctx context.Context, in terminalInput, workDir string) (agentsdk.ToolResult, error) {
	mode := t.Mode
	if mode == "" {
		mode = policy.PermissionModeDangerFullAccess
	}
	s, err := t.Manager.start(ctx, workDir, mode)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("failed to start terminal session: %v", err), IsError: true}, nil
	}
	terminalWait(ctx, clampTerminalWait(in.WaitMS), s)
	out, dropped := s.consumeNewOutput()
	return marshalTerminalSnapshot(s.snapshot(out, dropped))
}

func (t *TerminalTool) executeSend(ctx context.Context, in terminalInput) (agentsdk.ToolResult, error) {
	s, ok := t.Manager.get(strings.TrimSpace(in.SessionID))
	if !ok {
		return agentsdk.ToolResult{Content: "unknown session_id: " + in.SessionID + " (use op=start first, or op=list to see sessions)", IsError: true}, nil
	}
	if in.Keystrokes == "" {
		return agentsdk.ToolResult{Content: "keystrokes is required for op=send (end shell commands with \\n to execute them)", IsError: true}, nil
	}
	if err := s.send(in.Keystrokes); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("failed to send keystrokes: %v", err), IsError: true}, nil
	}
	terminalWait(ctx, clampTerminalWait(in.WaitMS), s)
	out, dropped := s.consumeNewOutput()
	return marshalTerminalSnapshot(s.snapshot(out, dropped))
}

func (t *TerminalTool) executeRead(ctx context.Context, in terminalInput) (agentsdk.ToolResult, error) {
	s, ok := t.Manager.get(strings.TrimSpace(in.SessionID))
	if !ok {
		return agentsdk.ToolResult{Content: "unknown session_id: " + in.SessionID + " (use op=start first, or op=list to see sessions)", IsError: true}, nil
	}
	if in.WaitMS > 0 {
		terminalWait(ctx, clampTerminalWait(in.WaitMS), s)
	}
	out, dropped := s.consumeNewOutput()
	return marshalTerminalSnapshot(s.snapshot(out, dropped))
}

func (t *TerminalTool) executeKill(in terminalInput) (agentsdk.ToolResult, error) {
	s, ok := t.Manager.get(strings.TrimSpace(in.SessionID))
	if !ok {
		return agentsdk.ToolResult{Content: "unknown session_id: " + in.SessionID, IsError: true}, nil
	}
	s.kill()
	out, dropped := s.consumeNewOutput()
	return marshalTerminalSnapshot(s.snapshot(out, dropped))
}

func (t *TerminalTool) executeList() (agentsdk.ToolResult, error) {
	sessions := t.Manager.list()
	snaps := make([]terminalSnapshot, 0, len(sessions))
	for _, s := range sessions {
		snaps = append(snaps, s.snapshot("", false))
	}
	data, err := json.MarshalIndent(snaps, "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}

func marshalTerminalSnapshot(snap terminalSnapshot) (agentsdk.ToolResult, error) {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}

func clampTerminalWait(waitMS int) time.Duration {
	if waitMS <= 0 {
		return terminalDefaultWait
	}
	wait := time.Duration(waitMS) * time.Millisecond
	if wait > terminalMaxWait {
		return terminalMaxWait
	}
	return wait
}

// terminalWait sleeps for the requested duration but returns early when the
// session exits or the tool context is cancelled.
func terminalWait(ctx context.Context, wait time.Duration, s *terminalSession) {
	if wait <= 0 {
		return
	}
	select {
	case <-time.After(wait):
	case <-s.done:
		// Give the read loop a moment to drain final output.
		time.Sleep(50 * time.Millisecond)
	case <-ctx.Done():
	}
}
