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

type fakeBrowserExecutorWithScreenshot struct {
	data []byte
}

func (e *fakeBrowserExecutorWithScreenshot) Build(context.Context, sandbox.Request) (*exec.Cmd, error) {
	return nil, nil
}

func (e *fakeBrowserExecutorWithScreenshot) Run(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
	for _, arg := range req.Argv {
		if path, ok := strings.CutPrefix(arg, "--screenshot="); ok {
			if err := os.WriteFile(path, e.data, 0o600); err != nil {
				return sandbox.Result{ExitCode: -1}, err
			}
		}
	}
	return sandbox.Result{}, nil
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

func TestScreenshotReplacesHardlinkWithoutChangingOutsideAlias(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "workspace")
	if err := os.Mkdir(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside.png")
	inside := filepath.Join(workDir, "shot.png")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, inside); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	executor := &fakeBrowserExecutorWithScreenshot{data: []byte("png-data")}
	result, err := (&Tool{Executor: executor}).screenshot(context.Background(), "chrome", input{
		URL:        "https://example.com",
		OutputPath: "shot.png",
	}, workDir, 800, 600)
	if err != nil || result.IsError {
		t.Fatalf("screenshot() result=%#v err=%v", result, err)
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "outside" {
		t.Fatalf("outside alias = %q, want unchanged", got)
	}
	got, err = os.ReadFile(inside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "png-data" {
		t.Fatalf("inside screenshot = %q", got)
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
	tool := &Tool{AllowPrivateNetworkURLs: true}
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

func TestExecutePublicOnlyFailsClosedBeforeBrowserLaunch(t *testing.T) {
	executor := &fakeBrowserExecutor{}
	result, err := (&Tool{Executor: executor}).Execute(context.Background(), json.RawMessage(`{"action":"navigate","url":"https://example.com"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "Use WebFetch") || !strings.Contains(result.Content, "AllowPrivateNetworkURLs=true") {
		t.Fatalf("result = %#v, want public-only containment guidance", result)
	}
	if len(executor.req.Argv) != 0 {
		t.Fatalf("browser launched with argv %#v", executor.req.Argv)
	}
}

func TestExecuteAllowsExplicitUnrestrictedBrowserNetworking(t *testing.T) {
	binDir := t.TempDir()
	chrome := filepath.Join(binDir, "chromium")
	if err := os.WriteFile(chrome, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	executor := &fakeBrowserExecutor{}
	result, err := (&Tool{AllowPrivateNetworkURLs: true, Executor: executor}).Execute(context.Background(), json.RawMessage(`{"action":"navigate","url":"https://93.184.216.34"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("result = %#v, want explicit unrestricted mode to launch", result)
	}
	if len(executor.req.Argv) == 0 || executor.req.Argv[0] != chrome {
		t.Fatalf("browser argv = %#v, want %q launch", executor.req.Argv, chrome)
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
	if !executor.req.AllowNetwork {
		t.Fatal("Browser request did not explicitly opt into network access")
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
