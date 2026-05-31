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
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

const (
	defaultBashTimeout    = 120 * time.Second
	maxBashTimeout        = 600 * time.Second
	defaultMaxOutputBytes = 100 * 1024
	bashDefaultTimeoutEnv = "GRATEFUL_BASH_DEFAULT_TIMEOUT_MS"
	bashMaxTimeoutEnv     = "GRATEFUL_BASH_MAX_TIMEOUT_MS"
	bashMaxOutputBytesEnv = "GRATEFUL_BASH_MAX_OUTPUT_BYTES"
	minBashTimeout        = time.Second
	minBashMaxOutputBytes = 4 * 1024
	maxBashMaxOutputBytes = 10 * 1024 * 1024
)

// BashTool executes shell commands.
type BashTool struct {
	Executor sandbox.Executor
}

type bashInput struct {
	Command     string `json:"command"`
	Timeout     int    `json:"timeout"`
	Description string `json:"description"`
}

func (t *BashTool) Name() string { return "Bash" }

func (t *BashTool) Description() string {
	return "Executes a bash command and returns its output (stdout and stderr combined). Use for running shell commands, build tools, git operations, and other CLI tasks."
}

func (t *BashTool) InputSchema() json.RawMessage {
	maxTimeout := int(effectiveMaxBashTimeout() / time.Millisecond)
	defaultTimeout := int(effectiveDefaultBashTimeout() / time.Millisecond)
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {
				"type": "string",
				"description": "The bash command to execute"
			},
			"timeout": {
				"type": "number",
				"description": "Timeout in milliseconds (max ` + strconv.Itoa(maxTimeout) + `, default ` + strconv.Itoa(defaultTimeout) + `)"
			},
			"description": {
				"type": "string",
				"description": "Description of what the command does"
			}
		},
		"required": ["command"]
	}`)
}

func (t *BashTool) IsReadOnly() bool { return false }

func (t *BashTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}

func (t *BashTool) NeedsApproval() bool { return false }

func (t *BashTool) TimeoutSeconds() int { return 0 }

func (t *BashTool) ToolForAccess(level agentsdk.ToolAccessLevel) agentsdk.Tool {
	if level == agentsdk.ToolAccessLevelReadOnly {
		return &ReadOnlyBashTool{BashTool: BashTool{Executor: t.Executor}}
	}
	return t
}

func (t *BashTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	return t.execute(ctx, input, workDir, policy.PermissionModeDangerFullAccess)
}

func (t *BashTool) execute(ctx context.Context, input json.RawMessage, workDir string, mode policy.PermissionMode) (agentsdk.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.Command == "" {
		return agentsdk.ToolResult{Content: "command is required", IsError: true}, nil
	}

	timeout := effectiveDefaultBashTimeout()
	maxTimeout := effectiveMaxBashTimeout()
	if in.Timeout > 0 {
		timeout = time.Duration(in.Timeout) * time.Millisecond
		if timeout > maxTimeout {
			timeout = maxTimeout
		}
	}

	executor := t.Executor
	if executor == nil {
		executor = sandbox.Default()
	}

	req := sandbox.Request{
		Argv:           []string{"bash", "--noprofile", "--norc", "-c", in.Command},
		WorkDir:        workDir,
		PermissionMode: mode,
		Timeout:        timeout,
	}

	maxOutputBytes := effectiveMaxBashOutputBytes()
	result, err := runBoundedRequest(ctx, executor, req, maxOutputBytes)
	outputStr := result.Output
	if result.Capped {
		outputStr += bashTruncationNotice(result.TotalBytes, maxOutputBytes)
	}

	if result.TimedOut {
		return agentsdk.ToolResult{Content: outputStr + "\n[command timed out]"}, nil
	}
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("%s\nError: %v", outputStr, err), IsError: true}, nil
	}
	if result.ExitCode != 0 {
		return agentsdk.ToolResult{Content: fmt.Sprintf("%s\nExit code: %d", outputStr, result.ExitCode)}, nil
	}
	if outputStr == "" {
		outputStr = "(no output)"
	}
	return agentsdk.ToolResult{Content: outputStr}, nil
}

// boundedResult is a sandbox.Result extended with an explicit Capped flag so
// callers can format truncation messages without re-scanning Output.
type boundedResult struct {
	Output     string
	ExitCode   int
	TimedOut   bool
	Capped     bool
	TotalBytes int64
}

// runBoundedRequest runs req under executor.Build, streaming combined
// stdout+stderr through a bounded buffer. When the byte cap is exceeded the
// process keeps running; only the tool response is truncated to a head/tail
// sample. This keeps verbose installers, training loops, and test runners from
// being killed merely because they logged too much.
func runBoundedRequest(ctx context.Context, executor sandbox.Executor, req sandbox.Request, cap int) (boundedResult, error) {
	cmd, err := executor.Build(ctx, req)
	if err != nil {
		return boundedResult{ExitCode: -1}, err
	}

	runCtx := ctx
	cancel := func() {}
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	// Re-bind the command without CommandContext's leader-only auto-kill so a
	// timeout can terminate the whole process group via configurePlatformProcAttrs.
	args := append([]string(nil), cmd.Args[1:]...)
	env := append([]string(nil), cmd.Env...)
	rebound := exec.Command(cmd.Path, args...)
	rebound.Dir = cmd.Dir
	rebound.Env = env
	configurePlatformProcAttrs(rebound)

	bw := newBoundedWriter(cap)
	attachBoundedPipes(rebound, bw)

	if err := rebound.Start(); err != nil {
		return boundedResult{ExitCode: -1}, err
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- rebound.Wait() }()

	timedOut := false
	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		killProcessTree(rebound)
		waitErr = <-waitDone
	}

	res := boundedResult{
		Output:     string(bw.Bytes()),
		Capped:     bw.Capped(),
		TotalBytes: bw.TotalBytes(),
	}
	if timedOut {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if waitErr == nil {
		res.ExitCode = 0
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	res.ExitCode = -1
	return res, waitErr
}

func effectiveDefaultBashTimeout() time.Duration {
	return envMillisDuration(bashDefaultTimeoutEnv, defaultBashTimeout, minBashTimeout)
}

func effectiveMaxBashTimeout() time.Duration {
	maxTimeout := envMillisDuration(bashMaxTimeoutEnv, maxBashTimeout, minBashTimeout)
	defaultTimeout := effectiveDefaultBashTimeout()
	if maxTimeout < defaultTimeout {
		return defaultTimeout
	}
	return maxTimeout
}

func effectiveMaxBashOutputBytes() int {
	return envBoundedInt(bashMaxOutputBytesEnv, defaultMaxOutputBytes, minBashMaxOutputBytes, maxBashMaxOutputBytes)
}

func envMillisDuration(key string, fallback, minValue time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	duration := time.Duration(parsed) * time.Millisecond
	if duration < minValue {
		return minValue
	}
	return duration
}

func envBoundedInt(key string, fallback, minValue, maxValue int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fallback
	}
	if parsed < minValue {
		return minValue
	}
	if parsed > maxValue {
		return maxValue
	}
	return parsed
}

func bashTruncationNotice(totalBytes int64, capBytes int) string {
	return fmt.Sprintf("\n[output truncated: captured head/tail from %d bytes; response cap %d bytes; process was not terminated]", totalBytes, capBytes)
}

// ReadOnlyBashTool is a Bash tool restricted to safe commands and read-only sandboxing.
type ReadOnlyBashTool struct {
	BashTool
}

func (t *ReadOnlyBashTool) IsReadOnly() bool { return true }

func (t *ReadOnlyBashTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ReadOnlyBashTool) ToolForAccess(agentsdk.ToolAccessLevel) agentsdk.Tool {
	return t
}

func (t *ReadOnlyBashTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	if blocked, reason := IsCommandBlockedForMode(policy.PermissionModeReadOnly, in.Command); blocked {
		return agentsdk.ToolResult{Content: reason, IsError: true}, nil
	}
	return t.BashTool.execute(ctx, input, workDir, policy.PermissionModeReadOnly)
}

// WorkspaceWriteBashTool allows local workspace edits while still blocking
// orchestrator-owned or clearly destructive shell commands.
type WorkspaceWriteBashTool struct {
	BashTool
}

func (t *WorkspaceWriteBashTool) ToolForAccess(level agentsdk.ToolAccessLevel) agentsdk.Tool {
	if level == agentsdk.ToolAccessLevelReadOnly {
		return &ReadOnlyBashTool{BashTool: t.BashTool}
	}
	return t
}

func (t *WorkspaceWriteBashTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in bashInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	if blocked, reason := IsCommandBlockedForMode(policy.PermissionModeWorkspaceWrite, in.Command); blocked {
		return agentsdk.ToolResult{Content: reason, IsError: true}, nil
	}
	return t.BashTool.execute(ctx, input, workDir, policy.PermissionModeWorkspaceWrite)
}

// IsCommandBlockedForMode is the public entry point for the Bash tools'
// destructive-command guard. It tokenizes the command (resolving quoting,
// escapes, $IFS, and ANSI-C $'...' decoding) before applying the per-mode
// policy, so naive evasions like \rm, "rm", $'\x2drf', ${IFS}, command
// substitution, and `bash -c "..."` are normalized first.
//
// NOTE on policy boundaries: this tool-layer denylist is defense in depth
// only. The primary read-only enforcement is the bubblewrap subprocess
// sandbox (see pkg/agentsdk/sandbox). A read-only command running outside the
// bubblewrap sandbox is a configuration error, not something this guard can
// safely cover.
func IsCommandBlockedForMode(mode policy.PermissionMode, command string) (bool, string) {
	readOnly, workspaceWrite := modeIsRestricted(mode)

	// Universal destructive-command checks (apply when not danger-full-access).
	if readOnly || workspaceWrite {
		if blocked, reason := classifyDestructive(command); blocked {
			return true, fmt.Sprintf("Command blocked in %s mode: %s", mode, reason)
		}
		// Any command substitution is suspicious in restricted modes.
		if hasAnyCommandSubstitution(command) {
			return true, fmt.Sprintf("Command blocked in %s mode: command substitution / backticks not allowed", mode)
		}
	}

	// Git policy.
	for _, argv := range gitInvocations(command) {
		if readOnly {
			if blocked, sub := isReadOnlyGitDenied(argv); blocked {
				return true, fmt.Sprintf("Command blocked in %s mode: git %s is not allowed", mode, sub)
			}
		}
		if workspaceWrite {
			if blocked, sub := isWorkspaceWriteGitDenied(argv); blocked {
				return true, fmt.Sprintf("Command blocked in %s mode: git %s is not allowed", mode, sub)
			}
		}
		if (readOnly || workspaceWrite) && pushTargetsProtectedBranch(argv) {
			return true, fmt.Sprintf("Command blocked in %s mode: push to main/master is not allowed", mode)
		}
	}

	return false, ""
}

func hasAnyCommandSubstitution(cmdLine string) bool {
	for _, t := range tokenize(cmdLine) {
		if t.HasCmdSub {
			return true
		}
	}
	return false
}

// IsPushToProtectedBranch detects git push commands that target main or
// master, including via shell wrappers and command substitution.
func IsPushToProtectedBranch(cmd string) bool {
	for _, argv := range gitInvocations(cmd) {
		if pushTargetsProtectedBranch(argv) {
			return true
		}
	}
	return false
}

func isProtectedRef(ref string) bool {
	ref = strings.TrimPrefix(ref, "+")
	if ref == "main" || ref == "master" || ref == "refs/heads/main" || ref == "refs/heads/master" {
		return true
	}
	if strings.HasPrefix(ref, "main:") || strings.HasPrefix(ref, "master:") {
		return true
	}
	return strings.HasSuffix(ref, ":main") ||
		strings.HasSuffix(ref, ":master") ||
		strings.HasSuffix(ref, ":refs/heads/main") ||
		strings.HasSuffix(ref, ":refs/heads/master")
}
