package tools

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/browser"
	sdkgit "github.com/gratefulagents/sdk/pkg/agentsdk/tools/git"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/lsp"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/shell"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/vision"
)

func TestRegistryOptionsHaveExplicitCapabilityClassification(t *testing.T) {
	classified := map[string]string{}
	for _, capability := range RegistryCapabilities() {
		for _, option := range capability.Options {
			if previous := classified[option]; previous != "" {
				t.Fatalf("registry option %s is classified by both %s and %s", option, previous, capability.Family)
			}
			classified[option] = capability.Family
		}
	}
	exemptConfigurationOptions := map[string]bool{
		"WithReadOnlyTools": true, "WithPermissionMode": true, "WithGitRemoteWrites": true,
		"WithBrowserScreenshotDir": true, "WithoutWebTools": true, "WithPrivateNetworkURLs": true,
		"WithCommandSandboxConfig": true, "WithAllowedMutatingTools": true,
	}
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(currentFile), "registry.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range parsed.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil || (!strings.HasPrefix(function.Name.Name, "With") && !strings.HasPrefix(function.Name.Name, "Without")) {
			continue
		}
		name := function.Name.Name
		if classified[name] == "" && !exemptConfigurationOptions[name] {
			t.Fatalf("registry option %s has no runtime-built-in, host-only, or configuration-only classification", name)
		}
	}
}

func TestNewRegistryDefaultTools(t *testing.T) {
	r := NewRegistry(t.TempDir())
	want := []string{"ApplyPatch", "Bash", "Delete", "Edit", "LSP", "Move", "WebFetch", "Write", "glob", "grep", "list_files", "read_file"}
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

func TestRegistryConfiguresLSPAndClosesIt(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithLSPConfig(lsp.Config{Command: "typescript-language-server", LanguageID: "typescript"}))
	tool, ok := r.Get("LSP").(*lsp.Tool)
	if !ok {
		t.Fatalf("LSP = %T, want *lsp.Tool", r.Get("LSP"))
	}
	if tool.Config.Command != "typescript-language-server" || tool.Config.LanguageID != "typescript" {
		t.Fatalf("LSP Config = %#v", tool.Config)
	}
	if closers := r.Closers(); len(closers) != 1 || closers[0] != tool {
		t.Fatalf("Closers() = %#v, want LSP tool", closers)
	}
}

func TestNewRegistryWithoutWebTools(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithoutWebTools())
	if r.Get("WebFetch") != nil {
		t.Fatalf("registry included WebFetch with WithoutWebTools; names=%v", r.Names())
	}
	for _, name := range []string{"ApplyPatch", "Bash", "Delete", "Edit", "LSP", "Move", "Write", "glob", "grep", "list_files", "read_file"} {
		if r.Get(name) == nil {
			t.Fatalf("registry missing %q with web disabled; names=%v", name, r.Names())
		}
	}
}

func TestRegistryBrowserUsesTrustedSandboxExecutor(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithBrowserTools(), WithPrivateNetworkURLs(true))
	tool, ok := r.Get("Browser").(*browser.Tool)
	if !ok {
		t.Fatalf("Browser tool = %T, want *browser.Tool", r.Get("Browser"))
	}
	if tool.Executor == nil {
		t.Fatal("Browser executor = nil")
	}
}

func TestRegistryConfiguresBrowserScreenshotDirectory(t *testing.T) {
	screenshotDir := t.TempDir()
	r := NewRegistry(
		t.TempDir(),
		WithBrowserTools(),
		WithBrowserScreenshotDir(screenshotDir),
		WithPrivateNetworkURLs(true),
		WithVisionTools(nil),
	)
	tool, ok := r.Get("Browser").(*browser.Tool)
	if !ok {
		t.Fatalf("Browser tool = %T, want *browser.Tool", r.Get("Browser"))
	}
	if tool.ScreenshotDir != screenshotDir {
		t.Fatalf("Browser ScreenshotDir = %q, want %q", tool.ScreenshotDir, screenshotDir)
	}
	visionTool, ok := r.Get("AnalyzeImage").(*vision.Tool)
	if !ok {
		t.Fatalf("AnalyzeImage tool = %T, want *vision.Tool", r.Get("AnalyzeImage"))
	}
	if len(visionTool.AllowedImageDirs) != 1 || visionTool.AllowedImageDirs[0] != screenshotDir {
		t.Fatalf("AnalyzeImage AllowedImageDirs = %#v, want [%q]", visionTool.AllowedImageDirs, screenshotDir)
	}
}

func TestRegistrySharesDefaultBrowserScreenshotDirectoryWithVision(t *testing.T) {
	r := NewRegistry(
		t.TempDir(),
		WithBrowserTools(),
		WithPrivateNetworkURLs(true),
		WithVisionTools(nil),
	)
	browserTool := r.Get("Browser").(*browser.Tool)
	visionTool := r.Get("AnalyzeImage").(*vision.Tool)
	if browserTool.ScreenshotDir != browser.DefaultScreenshotDir() {
		t.Fatalf("Browser ScreenshotDir = %q, want default %q", browserTool.ScreenshotDir, browser.DefaultScreenshotDir())
	}
	if len(visionTool.AllowedImageDirs) != 1 || visionTool.AllowedImageDirs[0] != browserTool.ScreenshotDir {
		t.Fatalf("AnalyzeImage AllowedImageDirs = %#v, want Browser screenshot dir", visionTool.AllowedImageDirs)
	}
}

func TestRegistryDoesNotRegisterUnconfinedBrowserByDefault(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithBrowserTools())
	if tool := r.Get("Browser"); tool != nil {
		t.Fatalf("Browser tool = %T, want nil without explicit private-network opt-in", tool)
	}
}

func TestRegistryAdaptsBrowserForReadOnlyMode(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithReadOnlyTools(), WithBrowserTools(), WithPrivateNetworkURLs(true))
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

func TestNewRegistryAlwaysWiresTrustedWorkspaceSandboxConfig(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(dir)
	if r.commandSandboxConfig == nil {
		t.Fatal("command sandbox config = nil")
	}
	if r.commandSandboxConfig.WorkspaceRoot != dir {
		t.Fatalf("WorkspaceRoot = %q, want %q", r.commandSandboxConfig.WorkspaceRoot, dir)
	}
	bash := r.Get("Bash").(*shell.WorkspaceWriteBashTool)
	if bash.Executor == nil {
		t.Fatal("default workspace-write Bash executor = nil")
	}
}

func TestNewRegistryWiresCommandSandboxConfigIntoBashTools(t *testing.T) {
	dir := t.TempDir()
	r := NewRegistry(dir, WithCommandSandboxConfig(sandbox.Config{Mode: "disabled", WorkspaceRoot: "/untrusted"}))
	if r.commandSandboxConfig.WorkspaceRoot != dir {
		t.Fatalf("WorkspaceRoot = %q, want trusted registry workdir %q", r.commandSandboxConfig.WorkspaceRoot, dir)
	}
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

func TestRegistryGitHubToolsRespectPermissionAndRemoteWritePolicy(t *testing.T) {
	readOnly := NewRegistry(t.TempDir(), WithReadOnlyTools(), WithGitHubPullRequestTool(nil, nil), WithGitHubIssueTool(nil, nil))
	if readOnly.Get("create_pull_request") != nil || readOnly.Get("create_github_issue") != nil {
		t.Fatalf("read-only registry exposed GitHub mutations: %v", readOnly.Names())
	}

	remoteWritesDisabled := NewRegistry(t.TempDir(),
		WithGitRemoteWrites(policy.GitRemoteWritesDisabled),
		WithGitHubPullRequestTool(nil, nil),
		WithGitHubIssueTool(nil, nil),
	)
	if remoteWritesDisabled.Get("create_pull_request") != nil {
		t.Fatalf("registry exposed pull-request tool with GitRemoteWrites disabled: %v", remoteWritesDisabled.Names())
	}
	if remoteWritesDisabled.Get("create_github_issue") == nil {
		t.Fatalf("registry omitted issue tool with GitRemoteWrites disabled: %v", remoteWritesDisabled.Names())
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

func TestInteractiveTerminalRequiresDangerFullAccess(t *testing.T) {
	for _, mode := range []policy.PermissionMode{policy.PermissionModeReadOnly, policy.PermissionModeWorkspaceWrite} {
		r := NewRegistry(t.TempDir(), WithPermissionMode(mode), WithInteractiveTerminal())
		if r.Get("Terminal") != nil {
			t.Fatalf("%s registry unexpectedly registered Terminal", mode)
		}
	}
	r := NewRegistry(t.TempDir(), WithPermissionMode(policy.PermissionModeDangerFullAccess), WithInteractiveTerminal())
	if r.Get("Terminal") == nil {
		t.Fatal("danger-full-access registry missing Terminal")
	}
}

func TestGitRemoteWritesDisabledConfiguresShellAndOmitsTerminal(t *testing.T) {
	r := NewRegistry(
		t.TempDir(),
		WithPermissionMode(policy.PermissionModeDangerFullAccess),
		WithGitRemoteWrites(policy.GitRemoteWritesDisabled),
		WithAsyncShellTools(),
		WithInteractiveTerminal(),
	)
	if r.Get("Terminal") != nil {
		t.Fatalf("Terminal registered with GitRemoteWrites disabled; names=%v", r.Names())
	}
	for _, tool := range []agentsdk.Tool{
		&shell.TerminalTool{},
		sdkgit.NewCreatePullRequestTool(nil, nil),
	} {
		r.Register(tool)
		if r.Get(tool.Name()) != nil {
			t.Fatalf("late registration retained Git remote-write tool %q", tool.Name())
		}
	}
	bash, ok := r.Get("Bash").(*shell.BashTool)
	if !ok || bash.GitRemoteWrites != policy.GitRemoteWritesDisabled {
		t.Fatalf("Bash = %#v, want GitRemoteWrites disabled", r.Get("Bash"))
	}
	start, ok := r.Get("BashStart").(*shell.BashStartTool)
	if !ok || start.GitRemoteWrites != policy.GitRemoteWritesDisabled {
		t.Fatalf("BashStart = %#v, want GitRemoteWrites disabled", r.Get("BashStart"))
	}
	result, err := bash.Execute(context.Background(), json.RawMessage(`{"command":"git push origin feature"}`), r.WorkDir())
	if err != nil || !result.IsError || !strings.Contains(result.Content, "GitRemoteWrites") {
		t.Fatalf("Bash push result = %+v err=%v, want GitRemoteWrites refusal", result, err)
	}
	result, err = start.Execute(context.Background(), json.RawMessage(`{"command":"git push origin feature"}`), r.WorkDir())
	if err != nil || !result.IsError || !strings.Contains(result.Content, "GitRemoteWrites") {
		t.Fatalf("BashStart push result = %+v err=%v, want GitRemoteWrites refusal", result, err)
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
	if closers := r.Closers(); len(closers) != 2 {
		t.Fatalf("Closers() len = %d, want 2", len(closers))
	}
}

func TestAsyncShellStartPollAndKill(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithPermissionMode(policy.PermissionModeDangerFullAccess), WithAsyncShellTools())
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

func TestBashStartKeepsRestrictedGitPolicy(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithAsyncShellTools())
	res, err := r.Get("BashStart").Execute(context.Background(), json.RawMessage(`{"command":"git push origin main"}`), r.WorkDir())
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content, "main/master") {
		t.Fatalf("BashStart result = %+v, want protected-branch refusal", res)
	}
}

func TestAsyncShellPollCanWait(t *testing.T) {
	r := NewRegistry(t.TempDir(), WithPermissionMode(policy.PermissionModeDangerFullAccess), WithAsyncShellTools())
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
