package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/web"
)

// Tool provides headless browser automation for agents.
type Tool struct {
	readOnly                bool
	AllowPrivateNetworkURLs bool
	Executor                sandbox.Executor
}

type input struct {
	Action     string `json:"action"`
	URL        string `json:"url"`
	Selector   string `json:"selector"`
	Text       string `json:"text"`
	Script     string `json:"script"`
	OutputPath string `json:"output_path"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
}

const (
	defaultViewportWidth  = 1280
	defaultViewportHeight = 720
	minViewportSize       = 100
	maxViewportSize       = 4096
)

func (t *Tool) Name() string { return "Browser" }

func (t *Tool) Description() string {
	if t.readOnly {
		return "Headless browser automation for read-only navigation and page text extraction. Screenshot capture requires workspace-write access."
	}
	return "Headless browser automation. Supports screenshot capture and navigation via headless Chromium. For full interactive automation (click, type, evaluate), configure the Playwright MCP server."
}

func (t *Tool) InputSchema() json.RawMessage {
	if t.readOnly {
		return json.RawMessage(`{
			"type": "object",
			"properties": {
				"action": {
					"type": "string",
					"enum": ["navigate", "get_text"],
					"description": "Browser action: navigate (load URL and return title), get_text (extract page text)"
				},
				"url": {
					"type": "string",
					"description": "URL to navigate to"
				},
				"width": {
					"type": "integer",
					"description": "Viewport width in pixels (default: 1280)"
				},
				"height": {
					"type": "integer",
					"description": "Viewport height in pixels (default: 720)"
				}
			},
			"required": ["action", "url"]
		}`)
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["screenshot", "navigate", "get_text"],
				"description": "Browser action: screenshot (capture page), navigate (load URL and return title), get_text (extract page text)"
			},
			"url": {
				"type": "string",
				"description": "URL to navigate to"
			},
			"output_path": {
				"type": "string",
				"description": "File path for screenshot output (default: .browser/screenshot-<timestamp>.png)"
			},
			"width": {
				"type": "integer",
				"description": "Viewport width in pixels (default: 1280)"
			},
			"height": {
				"type": "integer",
				"description": "Viewport height in pixels (default: 720)"
			}
		},
		"required": ["action", "url"]
	}`)
}

func (t *Tool) IsReadOnly() bool { return t.readOnly }

func (t *Tool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *Tool) NeedsApproval() bool { return false }

func (t *Tool) TimeoutSeconds() int { return 0 }

func (t *Tool) ToolForAccess(level agentsdk.ToolAccessLevel) agentsdk.Tool {
	if agentsdk.NormalizeToolAccessLevel(level) == agentsdk.ToolAccessLevelReadOnly {
		return &Tool{readOnly: true, AllowPrivateNetworkURLs: t.AllowPrivateNetworkURLs, Executor: t.Executor}
	}
	return t
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.URL == "" {
		return agentsdk.ToolResult{Content: "url is required", IsError: true}, nil
	}
	parsedURL, err := web.ValidateHTTPURL(ctx, in.URL, web.URLSecurityOptions{
		AllowPrivateNetworkURLs: t.AllowPrivateNetworkURLs,
	})
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	in.URL = parsedURL.String()
	if t.readOnly && in.Action == "screenshot" {
		return agentsdk.ToolResult{Content: "screenshot requires workspace-write access", IsError: true}, nil
	}

	width, height, err := normalizeViewport(in.Width, in.Height)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	switch in.Action {
	case "screenshot", "navigate", "get_text":
	default:
		return agentsdk.ToolResult{
			Content: fmt.Sprintf("Unknown action %q. Supported: screenshot, navigate, get_text. For click/type/evaluate, use the Playwright MCP server.", in.Action),
			IsError: true,
		}, nil
	}

	chromeBin := findChromeBinary()
	if chromeBin == "" {
		return agentsdk.ToolResult{
			Content: "No Chromium/Chrome binary found. Install Chromium or configure the Playwright MCP server for browser automation.",
			IsError: true,
		}, nil
	}

	switch in.Action {
	case "screenshot":
		return t.screenshot(ctx, chromeBin, in, workDir, width, height)
	case "navigate":
		return t.navigate(ctx, chromeBin, in, workDir, width, height)
	default:
		return t.getText(ctx, chromeBin, in, workDir, width, height)
	}
}

func normalizeViewport(width, height int) (int, int, error) {
	if width == 0 {
		width = defaultViewportWidth
	}
	if height == 0 {
		height = defaultViewportHeight
	}
	if width < minViewportSize || width > maxViewportSize {
		return 0, 0, fmt.Errorf("width must be between %d and %d pixels", minViewportSize, maxViewportSize)
	}
	if height < minViewportSize || height > maxViewportSize {
		return 0, 0, fmt.Errorf("height must be between %d and %d pixels", minViewportSize, maxViewportSize)
	}
	return width, height, nil
}

func (t *Tool) screenshot(ctx context.Context, chromeBin string, in input, workDir string, width, height int) (agentsdk.ToolResult, error) {
	if strings.TrimSpace(workDir) == "" {
		return agentsdk.ToolResult{Content: "workDir is required for screenshots", IsError: true}, nil
	}
	outPath := in.OutputPath
	if outPath == "" {
		var err error
		outPath, err = pathutil.ResolveWorkspace(workDir, filepath.Join(".browser", fmt.Sprintf("screenshot-%d.png", time.Now().UnixNano())))
		if err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to resolve screenshot path: %v", err), IsError: true}, nil
		}
	} else {
		var err error
		outPath, err = pathutil.ResolveWorkspace(workDir, outPath)
		if err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to resolve output path: %v", err), IsError: true}, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to create output directory: %v", err), IsError: true}, nil
	}
	tmpFile, err := os.CreateTemp(filepath.Dir(outPath), ".screenshot-*.png")
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to create temporary screenshot file: %v", err), IsError: true}, nil
	}
	tmpPath := tmpFile.Name()
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to close temporary screenshot file: %v", err), IsError: true}, nil
	}
	defer os.Remove(tmpPath)

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"--headless",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-sandbox",
		fmt.Sprintf("--screenshot=%s", tmpPath),
		fmt.Sprintf("--window-size=%d,%d", width, height),
		"--hide-scrollbars",
		in.URL,
	}

	execResult, err := t.executor().Run(ctx, sandbox.Request{
		Argv:           append([]string{chromeBin}, args...),
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeWorkspaceWrite,
		Timeout:        30 * time.Second,
	})
	if err != nil || execResult.ExitCode != 0 || execResult.TimedOut {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Screenshot failed: %v\n%s", err, execResult.Output), IsError: true}, nil
	}
	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to read temporary screenshot file: %v", err), IsError: true}, nil
	}
	if err := pathutil.WriteFileNoFollow(outPath, data, 0o600); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to save screenshot: %v", err), IsError: true}, nil
	}

	basePath, _ := pathutil.ResolveWorkspace(workDir, ".")
	relPath, _ := filepath.Rel(basePath, outPath)
	if relPath == "" {
		relPath = outPath
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Screenshot saved to %s (%dx%d)", relPath, width, height)}, nil
}

func (t *Tool) navigate(ctx context.Context, chromeBin string, in input, workDir string, width, height int) (agentsdk.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"--headless",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-sandbox",
		"--dump-dom",
		fmt.Sprintf("--window-size=%d,%d", width, height),
		in.URL,
	}

	execResult, err := t.executor().Run(ctx, sandbox.Request{
		Argv:           append([]string{chromeBin}, args...),
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeReadOnly,
		Timeout:        30 * time.Second,
	})
	if err != nil || execResult.ExitCode != 0 || execResult.TimedOut {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Navigation failed: %v\n%s", err, execResult.Output), IsError: true}, nil
	}

	title := extractHTMLTitle(execResult.Output)
	if title == "" {
		title = "(no title)"
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Navigated to %s\nTitle: %s", in.URL, title)}, nil
}

func (t *Tool) getText(ctx context.Context, chromeBin string, in input, workDir string, width, height int) (agentsdk.ToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	args := []string{
		"--headless",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-sandbox",
		"--dump-dom",
		fmt.Sprintf("--window-size=%d,%d", width, height),
		in.URL,
	}

	execResult, err := t.executor().Run(ctx, sandbox.Request{
		Argv:           append([]string{chromeBin}, args...),
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeReadOnly,
		Timeout:        30 * time.Second,
	})
	if err != nil || execResult.ExitCode != 0 || execResult.TimedOut {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to get page text: %v\n%s", err, execResult.Output), IsError: true}, nil
	}

	text := web.HTMLToText(execResult.Output)
	if len(text) > 50000 {
		text = text[:50000] + "\n\n--- Content truncated at 50000 characters ---"
	}
	return agentsdk.ToolResult{Content: text}, nil
}

func (t *Tool) executor() sandbox.Executor {
	if t.Executor != nil {
		return t.Executor
	}
	config := sandbox.ConfigFromEnv()
	config.Mode = "required"
	return sandbox.DefaultWithConfig(config)
}

func findChromeBinary() string {
	candidates := []string{
		"chromium",
		"chromium-browser",
		"google-chrome",
		"google-chrome-stable",
		"/usr/bin/chromium",
		"/usr/bin/chromium-browser",
		"/usr/bin/google-chrome",
		"/usr/bin/google-chrome-stable",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path
		}
	}
	return ""
}

func extractHTMLTitle(rawHTML string) string {
	lower := strings.ToLower(rawHTML)
	start := strings.Index(lower, "<title")
	if start < 0 {
		return ""
	}
	tagEnd := strings.Index(lower[start:], ">")
	if tagEnd < 0 {
		return ""
	}
	contentStart := start + tagEnd + 1
	end := strings.Index(lower[contentStart:], "</title>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rawHTML[contentStart : contentStart+end])
}
