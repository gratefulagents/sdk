package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

// AsyncManager owns background shell jobs started by the async Bash tools.
type AsyncManager struct {
	mu       sync.Mutex
	nextID   int
	executor sandbox.Executor
	jobs     map[string]*asyncJob
}

type asyncJob struct {
	id          string
	command     string
	description string
	startedAt   time.Time
	timeout     time.Duration
	cmd         *exec.Cmd
	cancel      context.CancelFunc
	output      *boundedWriter
	done        chan struct{}

	mu       sync.Mutex
	exitCode int
	timedOut bool
	err      error
	endedAt  time.Time
}

// NewAsyncManager creates a manager for async shell jobs.
func NewAsyncManager(executor sandbox.Executor) *AsyncManager {
	return &AsyncManager{executor: executor, jobs: map[string]*asyncJob{}}
}

// Close terminates all running jobs.
func (m *AsyncManager) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	jobs := make([]*asyncJob, 0, len(m.jobs))
	for _, job := range m.jobs {
		jobs = append(jobs, job)
	}
	m.mu.Unlock()
	for _, job := range jobs {
		job.kill()
	}
	return nil
}

func (m *AsyncManager) start(ctx context.Context, in asyncStartInput, workDir string, mode policy.PermissionMode) (string, error) {
	if m == nil {
		return "", errors.New("async shell manager is nil")
	}
	executor := m.executor
	if executor == nil {
		executor = sandbox.Default()
	}
	timeout := effectiveMaxBashTimeout()
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Millisecond
		if timeout > effectiveMaxBashTimeout() {
			timeout = effectiveMaxBashTimeout()
		}
	}
	req := sandbox.Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", in.Command},
		WorkDir:        workDir,
		PermissionMode: mode,
	}
	built, err := executor.Build(ctx, req)
	if err != nil {
		return "", err
	}

	args := append([]string(nil), built.Args[1:]...)
	env := append([]string(nil), built.Env...)
	cmd := exec.Command(built.Path, args...)
	cmd.Dir = built.Dir
	cmd.Env = env
	configurePlatformProcAttrs(cmd)

	output := newBoundedWriter(effectiveMaxBashOutputBytes())
	attachBoundedPipes(cmd, output)

	runCtx := context.Background()
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(runCtx, timeout)
	}

	m.mu.Lock()
	m.sweepFinishedLocked()
	m.nextID++
	id := "bash-" + strconv.Itoa(m.nextID)
	job := &asyncJob{
		id:          id,
		command:     in.Command,
		description: strings.TrimSpace(in.Description),
		startedAt:   time.Now(),
		timeout:     timeout,
		cmd:         cmd,
		cancel:      cancel,
		output:      output,
		done:        make(chan struct{}),
		exitCode:    -1,
	}
	m.jobs[id] = job
	m.mu.Unlock()

	if err := cmd.Start(); err != nil {
		cancel()
		m.mu.Lock()
		delete(m.jobs, id)
		m.mu.Unlock()
		return "", err
	}

	go job.wait(runCtx)
	return id, nil
}

func (m *AsyncManager) get(id string) (*asyncJob, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[id]
	return job, ok
}

// asyncJobRetention bounds how long a finished async job is retained so its
// final status and output remain pollable, after which it is evicted to keep
// the jobs map from growing without bound over a long-lived session.
const asyncJobRetention = 30 * time.Minute

// sweepFinishedLocked evicts finished jobs whose output has been available for
// longer than asyncJobRetention. Callers must hold m.mu.
func (m *AsyncManager) sweepFinishedLocked() {
	now := time.Now()
	for id, job := range m.jobs {
		select {
		case <-job.done:
		default:
			continue
		}
		job.mu.Lock()
		endedAt := job.endedAt
		job.mu.Unlock()
		if !endedAt.IsZero() && now.Sub(endedAt) > asyncJobRetention {
			delete(m.jobs, id)
		}
	}
}

func (j *asyncJob) wait(ctx context.Context) {
	waitDone := make(chan error, 1)
	go func() { waitDone <- j.cmd.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		timedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		killProcessTree(j.cmd)
		waitErr = <-waitDone
	}
	j.cancel()

	j.mu.Lock()
	defer j.mu.Unlock()
	j.timedOut = timedOut
	j.endedAt = time.Now()
	if timedOut {
		j.exitCode = -1
		j.err = nil
		close(j.done)
		return
	}
	if waitErr == nil {
		j.exitCode = 0
		close(j.done)
		return
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		j.exitCode = exitErr.ExitCode()
		j.err = nil
		close(j.done)
		return
	}
	j.exitCode = -1
	j.err = waitErr
	close(j.done)
}

func (j *asyncJob) kill() {
	if j == nil {
		return
	}
	select {
	case <-j.done:
		return
	default:
	}
	j.cancel()
	killProcessTree(j.cmd)
	<-j.done
}

func (j *asyncJob) snapshot() asyncJobSnapshot {
	j.mu.Lock()
	exitCode := j.exitCode
	timedOut := j.timedOut
	err := j.err
	endedAt := j.endedAt
	j.mu.Unlock()

	running := true
	select {
	case <-j.done:
		running = false
	default:
	}

	output := string(j.output.Bytes())
	if output == "" {
		output = "(no output yet)"
	}
	if j.output.Capped() {
		output += bashTruncationNotice(j.output.TotalBytes(), effectiveMaxBashOutputBytes())
	}

	status := "running"
	if !running {
		status = "exited"
		if timedOut {
			status = "timed_out"
		} else if err != nil {
			status = "error"
		}
	}
	elapsed := time.Since(j.startedAt)
	if !endedAt.IsZero() {
		elapsed = endedAt.Sub(j.startedAt)
	}
	return asyncJobSnapshot{
		ID:          j.id,
		Status:      status,
		Running:     running,
		ExitCode:    exitCode,
		TimedOut:    timedOut,
		Error:       errorString(err),
		StartedAt:   j.startedAt.Format(time.RFC3339),
		EndedAt:     formatOptionalTime(endedAt),
		ElapsedMS:   elapsed.Milliseconds(),
		TimeoutMS:   j.timeout.Milliseconds(),
		Description: j.description,
		Output:      output,
	}
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

type asyncStartInput struct {
	Command     string `json:"command"`
	Timeout     int    `json:"timeout"`
	Description string `json:"description"`
}

type asyncIDInput struct {
	ID     string `json:"id"`
	WaitMS int    `json:"wait_ms"`
}

type asyncJobSnapshot struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Running     bool   `json:"running"`
	ExitCode    int    `json:"exit_code"`
	TimedOut    bool   `json:"timed_out"`
	Error       string `json:"error,omitempty"`
	StartedAt   string `json:"started_at"`
	EndedAt     string `json:"ended_at,omitempty"`
	ElapsedMS   int64  `json:"elapsed_ms"`
	TimeoutMS   int64  `json:"timeout_ms"`
	Description string `json:"description,omitempty"`
	Output      string `json:"output"`
}

// BashStartTool starts a long-running shell command in the background.
type BashStartTool struct {
	Manager *AsyncManager
	Mode    policy.PermissionMode
}

func (t *BashStartTool) Name() string { return "BashStart" }

func (t *BashStartTool) Description() string {
	return "Starts a long-running bash command in the background and returns a job id. Use with BashPoll for builds, training, simulations, OCR, and other commands that may run for a long time."
}

func (t *BashStartTool) InputSchema() json.RawMessage {
	maxTimeout := int(effectiveMaxBashTimeout() / time.Millisecond)
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "The bash command to execute"},
			"timeout": {"type": "number", "description": "Timeout in milliseconds (max ` + strconv.Itoa(maxTimeout) + `, default ` + strconv.Itoa(maxTimeout) + `)"},
			"description": {"type": "string", "description": "Description of what the command does"}
		},
		"required": ["command"]
	}`)
}

func (t *BashStartTool) IsReadOnly() bool { return false }
func (t *BashStartTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}
func (t *BashStartTool) NeedsApproval() bool { return false }
func (t *BashStartTool) TimeoutSeconds() int { return 0 }

func (t *BashStartTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in asyncStartInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if strings.TrimSpace(in.Command) == "" {
		return agentsdk.ToolResult{Content: "command is required", IsError: true}, nil
	}
	mode := t.Mode
	if mode == "" {
		mode = policy.PermissionModeDangerFullAccess
	}
	id, err := t.Manager.start(ctx, in, workDir, mode)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: "started background bash job " + id}, nil
}

// BashPollTool polls a background shell job.
type BashPollTool struct {
	Manager *AsyncManager
}

func (t *BashPollTool) Name() string { return "BashPoll" }

func (t *BashPollTool) Description() string {
	return "Polls a background bash job started by BashStart and returns status plus bounded recent output. Optionally waits before returning so long jobs do not require tight polling."
}

func (t *BashPollTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Job id returned by BashStart"},
			"wait_ms": {"type": "number", "description": "Optional milliseconds to wait for the job to finish before returning, max 120000"}
		},
		"required": ["id"]
	}`)
}

func (t *BashPollTool) IsReadOnly() bool { return true }
func (t *BashPollTool) IsEnabled(context *agentsdk.RunContext) bool {
	return true
}
func (t *BashPollTool) NeedsApproval() bool { return false }
func (t *BashPollTool) TimeoutSeconds() int { return 0 }

func (t *BashPollTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in asyncIDInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	job, ok := t.Manager.get(strings.TrimSpace(in.ID))
	if !ok {
		return agentsdk.ToolResult{Content: "unknown job id: " + in.ID, IsError: true}, nil
	}
	if wait := clampPollWait(in.WaitMS); wait > 0 {
		select {
		case <-job.done:
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
	data, err := json.MarshalIndent(job.snapshot(), "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}

func clampPollWait(waitMS int) time.Duration {
	if waitMS <= 0 {
		return 0
	}
	wait := time.Duration(waitMS) * time.Millisecond
	if wait > 2*time.Minute {
		return 2 * time.Minute
	}
	return wait
}

// BashKillTool terminates a background shell job.
type BashKillTool struct {
	Manager *AsyncManager
}

func (t *BashKillTool) Name() string { return "BashKill" }

func (t *BashKillTool) Description() string {
	return "Terminates a background bash job started by BashStart."
}

func (t *BashKillTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"Job id returned by BashStart"}},"required":["id"]}`)
}

func (t *BashKillTool) IsReadOnly() bool { return false }
func (t *BashKillTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}
func (t *BashKillTool) NeedsApproval() bool { return false }
func (t *BashKillTool) TimeoutSeconds() int { return 0 }

func (t *BashKillTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in asyncIDInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	job, ok := t.Manager.get(strings.TrimSpace(in.ID))
	if !ok {
		return agentsdk.ToolResult{Content: "unknown job id: " + in.ID, IsError: true}, nil
	}
	job.kill()
	data, err := json.MarshalIndent(job.snapshot(), "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}
