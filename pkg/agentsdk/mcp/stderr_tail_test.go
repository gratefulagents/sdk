package mcp

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

// directExecutor runs commands without a sandbox so tests can exercise real
// child processes deterministically. It mirrors the real executors' use of
// exec.CommandContext so context-lifetime regressions reproduce.
type directExecutor struct{}

func (directExecutor) Build(ctx context.Context, req sandbox.Request) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.WorkDir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd, nil
}

func (directExecutor) Run(context.Context, sandbox.Request) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}

func TestStderrTailKeepsBoundedTail(t *testing.T) {
	t.Parallel()

	tail := newStderrTail(8)
	if _, err := tail.Write([]byte("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	if _, err := tail.Write([]byte("XYZ")); err != nil {
		t.Fatal(err)
	}
	if got := tail.Tail(); got != "defghXYZ" {
		t.Fatalf("Tail() = %q, want trailing 8 bytes %q", got, "defghXYZ")
	}
	var nilTail *stderrTail
	if nilTail.Tail() != "" {
		t.Fatal("nil tail must read empty")
	}
}

func TestErrWithStderr(t *testing.T) {
	t.Parallel()

	if errWithStderr(nil, "x") != nil {
		t.Fatal("nil error must stay nil")
	}
	base := context.DeadlineExceeded
	if got := errWithStderr(base, ""); got != base {
		t.Fatal("empty tail must not wrap")
	}
	got := errWithStderr(base, "boom")
	if !strings.Contains(got.Error(), "server stderr: boom") {
		t.Fatalf("annotated error = %v", got)
	}
}

// TestConnectStdioServerReportsChildStderr is the regression test for blind
// "initialize: EOF" failures: a child that dies at startup must surface what
// it wrote to stderr in the connect error.
func TestConnectStdioServerReportsChildStderr(t *testing.T) {
	t.Parallel()

	opts := resolveManagerOptions(t.TempDir(), WithCommandExecutor(directExecutor{}))
	cfg := ServerConfig{Command: "sh", Args: []string{"-c", "echo boom-traceback >&2; exit 3"}}
	_, err := connectStdioServer(context.Background(), t.TempDir(), "crashy", cfg, opts)
	if err == nil {
		t.Fatal("expected connect error for crashing child")
	}
	if !strings.Contains(err.Error(), "boom-traceback") {
		t.Fatalf("connect error missing child stderr: %v", err)
	}
}
