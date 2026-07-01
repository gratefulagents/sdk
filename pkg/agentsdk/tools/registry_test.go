package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	sdkgit "github.com/gratefulagents/sdk/pkg/agentsdk/tools/git"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/shell"
)

func TestNewRegistryDefaultTools(t *testing.T) {
	r := NewRegistry(t.TempDir())
	want := []string{"Bash", "Edit", "LSP", "WebFetch", "Write", "glob", "grep", "list_files", "read_file"}
	got := r.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
}

func TestNewRegistryWithoutWebTools(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithoutWebTools())
	if r.Get("WebFetch") != nil {
		t.Fatalf("registry included WebFetch with WithoutWebTools; names=%v", r.Names())
	}
	for _, name := range []string{"Bash", "Edit", "LSP", "Write", "glob", "grep", "list_files", "read_file"} {
		if r.Get(name) == nil {
			t.Fatalf("registry missing %q with web disabled; names=%v", name, r.Names())
		}
	}
}

func TestRegistryAdaptsBrowserForReadOnlyMode(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithReadOnlyTools(), WithBrowserTools())
	tool := r.Get("Browser")
	if tool == nil {
		t.Fatalf("read-only registry missing Browser; names=%v", r.Names())
	}
	if !tool.IsReadOnly() {
		t.Fatalf("Browser readOnly=false in read-only registry")
	}
	if strings.Contains(string(tool.InputSchema()), "screenshot") {
		t.Fatalf("read-only Browser schema advertised screenshot: %s", tool.InputSchema())
	}
}

func TestNewRegistryReadOnly(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithReadOnlyTools(), WithSignalTools())
	want := []string{"AskUserQuestion", "Bash", "LSP", "WebFetch", "glob", "grep", "list_files", "present_plan", "read_file"}
	got := r.Names()
	if len(got) != len(want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names() = %v, want %v", got, want)
		}
	}
	if _, ok := r.Get("Bash").(*shell.ReadOnlyBashTool); !ok {
		t.Fatalf("Bash tool = %T, want *shell.ReadOnlyBashTool", r.Get("Bash"))
	}
}

func TestNewRegistryWiresCommandSandboxConfigIntoBashTools(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithCommandSandboxConfig(sandbox.Config{Mode: "disabled"}))
	bash, ok := r.Get("Bash").(*shell.WorkspaceWriteBashTool)
	if !ok {
		t.Fatalf("Bash tool = %T, want *shell.WorkspaceWriteBashTool", r.Get("Bash"))
	}
	if bash.Executor == nil {
		t.Fatal("workspace-write Bash executor = nil, want configured executor")
	}

	readOnly := NewRegistry(t.TempDir(), WithReadOnlyTools(), WithCommandSandboxConfig(sandbox.Config{Mode: "disabled"}))
	roBash, ok := readOnly.Get("Bash").(*shell.ReadOnlyBashTool)
	if !ok {
		t.Fatalf("read-only Bash tool = %T, want *shell.ReadOnlyBashTool", readOnly.Get("Bash"))
	}
	if roBash.Executor == nil {
		t.Fatal("read-only Bash executor = nil, want configured executor")
	}
}

func TestNewRegistryRetainsWorkDir(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(dir)
	if got := r.WorkDir(); got != dir {
		t.Fatalf("WorkDir() = %q, want %q", got, dir)
	}
}

func TestNewRegistryWithAttachRepositoryTool(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithAttachRepositoryTool(sdkgit.WithAttachRepositoryDefaultBaseBranch("main")))
	tool := r.Get("attach_repository")
	if tool == nil {
		t.Fatalf("registry missing attach_repository; names=%v", r.Names())
	}
	if tool.IsReadOnly() {
		t.Fatalf("attach_repository reported read-only")
	}
}

func TestNewRegistryDangerFullAccess(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithPermissionMode(policy.PermissionModeDangerFullAccess))
	if got := r.PermissionMode(); got != policy.PermissionModeDangerFullAccess {
		t.Fatalf("PermissionMode() = %q, want %q", got, policy.PermissionModeDangerFullAccess)
	}
	for _, name := range []string{"Bash", "Edit", "Write"} {
		if r.Get(name) == nil {
			t.Fatalf("danger-full-access registry missing %q", name)
		}
	}
}

func TestNewRegistryAsyncShellTools(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithAsyncShellTools())
	want := []string{"BashKill", "BashPoll", "BashStart"}
	for _, name := range want {
		if r.Get(name) == nil {
			t.Fatalf("registry missing async shell tool %q; names=%v", name, r.Names())
		}
	}
	if closers := r.Closers(); len(closers) != 1 {
		t.Fatalf("Closers() len = %d, want 1", len(closers))
	}
}

func TestAsyncShellStartPollAndKill(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithAsyncShellTools())
	start := r.Get("BashStart")
	poll := r.Get("BashPoll")
	kill := r.Get("BashKill")

	started, err := start.Execute(context.Background(), json.RawMessage(`{"command":"printf ready; sleep 5"}`), r.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if started.IsError {
		t.Fatalf("BashStart error: %s", started.Content)
	}
	fields := strings.Fields(started.Content)
	jobID := fields[len(fields)-1]

	deadline := time.Now().Add(2 * time.Second)
	for {
		polled, err := poll.Execute(context.Background(), json.RawMessage(`{"id":"`+jobID+`"}`), r.WorkDir())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(polled.Content, "ready") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("poll did not show output before deadline: %s", polled.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}

	killed, err := kill.Execute(context.Background(), json.RawMessage(`{"id":"`+jobID+`"}`), r.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if killed.IsError || !strings.Contains(killed.Content, `"running": false`) {
		t.Fatalf("BashKill result = error=%v content=%s", killed.IsError, killed.Content)
	}
}

func TestAsyncShellPollCanWait(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithAsyncShellTools())
	start := r.Get("BashStart")
	poll := r.Get("BashPoll")

	started, err := start.Execute(context.Background(), json.RawMessage(`{"command":"sleep 0.05; printf done"}`), r.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if started.IsError {
		t.Fatalf("BashStart error: %s", started.Content)
	}
	fields := strings.Fields(started.Content)
	jobID := fields[len(fields)-1]

	polled, err := poll.Execute(context.Background(), json.RawMessage(`{"id":"`+jobID+`","wait_ms":500}`), r.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(polled.Content, `"running": false`) || !strings.Contains(polled.Content, "done") {
		t.Fatalf("BashPoll wait result = %s", polled.Content)
	}
}
