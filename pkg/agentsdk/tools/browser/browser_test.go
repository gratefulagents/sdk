package browser

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

type fakeBrowserExecutor struct {
	req sandbox.Request
}

func (e *fakeBrowserExecutor) Build(context.Context, sandbox.Request) (*exec.Cmd, error) {
	return nil, nil
}

func (e *fakeBrowserExecutor) Run(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
	e.req = req
	return sandbox.Result{Output: "<html><head><title>OK</title></head><body>Hello</body></html>"}, nil
}

func TestScreenshotRejectsOutputPathEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := (&Tool{}).screenshot(context.Background(), "chrome", input{
		URL:        "https://example.com",
		OutputPath: "../outside.png",
	}, workDir, 800, 600)
	if err != nil {
		t.Fatalf("screenshot() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "outside the workspace root") {
		t.Fatalf("result = %#v, want workspace escape rejection", result)
	}
}

func TestScreenshotRejectsSymlinkOutputDirectoryEscape(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	outsideDir := filepath.Join(root, "outside")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(workDir, "link")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	result, err := (&Tool{}).screenshot(context.Background(), "chrome", input{
		URL:        "https://example.com",
		OutputPath: filepath.Join("link", "shot.png"),
	}, workDir, 800, 600)
	if err != nil {
		t.Fatalf("screenshot() error = %v", err)
	}
	if !result.IsError || !strings.Contains(result.Content, "outside the workspace root") {
		t.Fatalf("result = %#v, want workspace escape rejection", result)
	}
}

func TestToolForReadOnlyAccessRemovesScreenshotCapability(t *testing.T) {
	tool := &Tool{}
	if tool.IsReadOnly() {
		t.Fatal("default Browser tool should be write-capable because screenshots create files")
	}

	adapted := tool.ToolForAccess(agentsdk.ToolAccessLevelReadOnly)
	if adapted == nil || !adapted.IsReadOnly() {
		t.Fatalf("adapted tool = %#v, want read-only Browser", adapted)
	}
	if strings.Contains(string(adapted.InputSchema()), "screenshot") {
		t.Fatalf("read-only Browser schema should not advertise screenshot: %s", adapted.InputSchema())
	}

	result, err := adapted.Execute(context.Background(), json.RawMessage(`{"action":"screenshot","url":"https://93.184.216.34","output_path":"shot.png"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "workspace-write access") {
		t.Fatalf("result = %#v, want read-only screenshot rejection", result)
	}
}

func TestToolForReadOnlyAccessPreservesExecutor(t *testing.T) {
	executor := &fakeBrowserExecutor{}
	tool := &Tool{Executor: executor}

	adapted, ok := tool.ToolForAccess(agentsdk.ToolAccessLevelReadOnly).(*Tool)
	if !ok {
		t.Fatalf("adapted tool type = %T, want *Tool", adapted)
	}
	if adapted.Executor != executor {
		t.Fatal("read-only Browser adapter did not preserve executor")
	}
}

func TestNavigateUsesConfiguredSandboxExecutor(t *testing.T) {
	executor := &fakeBrowserExecutor{}
	tool := &Tool{Executor: executor}

	result, err := tool.navigate(context.Background(), "chrome", input{URL: "https://example.com"}, t.TempDir(), 800, 600)
	if err != nil {
		t.Fatalf("navigate() error = %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "Title: OK") {
		t.Fatalf("result = %#v, want successful fake navigation", result)
	}
	if executor.req.PermissionMode != policy.PermissionModeReadOnly {
		t.Fatalf("PermissionMode = %q, want read-only", executor.req.PermissionMode)
	}
	if len(executor.req.Argv) == 0 || executor.req.Argv[0] != "chrome" {
		t.Fatalf("Argv = %#v, want chrome command", executor.req.Argv)
	}
}

func TestNormalizeViewportBounds(t *testing.T) {
	width, height, err := normalizeViewport(0, 0)
	if err != nil {
		t.Fatalf("normalizeViewport() error = %v", err)
	}
	if width != defaultViewportWidth || height != defaultViewportHeight {
		t.Fatalf("normalizeViewport() = %dx%d, want defaults", width, height)
	}
	if _, _, err := normalizeViewport(maxViewportSize+1, 720); err == nil {
		t.Fatal("normalizeViewport() accepted oversized width")
	}
	if _, _, err := normalizeViewport(1280, minViewportSize-1); err == nil {
		t.Fatal("normalizeViewport() accepted undersized height")
	}
}
