package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

var writeFileNoFollow = pathutil.WriteFileNoFollow

// FileWriteTool creates or overwrites files.
type FileWriteTool struct{}

type fileWriteInput struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

func (t *FileWriteTool) resolvePath(workDir, inputPath string) (string, error) {
	return pathutil.Resolve(workDir, inputPath), nil
}

// WorkspaceWriteFileTool restricts writes to the agent workspace.
type WorkspaceWriteFileTool struct {
	FileWriteTool
}

func (t *WorkspaceWriteFileTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in fileWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.FilePath == "" {
		return agentsdk.ToolResult{Content: "file_path is required", IsError: true}, nil
	}
	// Validate workspace membership and reject symlink components up front so we
	// can return a friendly error with the canonical path. The actual write goes
	// through OpenInWorkspace, which on Linux uses openat2(2) with
	// RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS to atomically reject any path that
	// escapes the workspace — closing the canonicalize-then-open TOCTOU window.
	canonical, err := pathutil.ResolveWorkspace(workDir, in.FilePath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}
	dir := filepath.Dir(canonical)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error creating directory %s: %v", dir, err), IsError: true}, nil
	}
	f, err := pathutil.OpenInWorkspace(workDir, canonical, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	if _, err := f.Write([]byte(in.Content)); err != nil {
		_ = f.Close()
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	if err := f.Close(); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error closing file: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Content), canonical)}, nil
}

func (t *FileWriteTool) Name() string { return "Write" }

func (t *FileWriteTool) Description() string {
	return "Creates or overwrites a file with the given content. Creates parent directories if needed."
}

func (t *FileWriteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "Absolute path to the file to write"
			},
			"content": {
				"type": "string",
				"description": "Content to write to the file"
			}
		},
		"required": ["file_path", "content"]
	}`)
}

func (t *FileWriteTool) IsReadOnly() bool { return false }

func (t *FileWriteTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}

func (t *FileWriteTool) NeedsApproval() bool { return false }

func (t *FileWriteTool) TimeoutSeconds() int { return 0 }

func (t *FileWriteTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	return executeFileWrite(ctx, input, workDir, t.resolvePath)
}

func executeFileWrite(ctx context.Context, input json.RawMessage, workDir string, resolvePath func(string, string) (string, error)) (agentsdk.ToolResult, error) {
	var in fileWriteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.FilePath == "" {
		return agentsdk.ToolResult{Content: "file_path is required", IsError: true}, nil
	}

	resolvedPath, err := resolvePath(workDir, in.FilePath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}

	dir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error creating directory %s: %v", dir, err), IsError: true}, nil
	}
	resolvedPath, err = resolvePath(workDir, resolvedPath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}
	if err := writeFileNoFollow(resolvedPath, []byte(in.Content), 0o644); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}

	return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully wrote %d bytes to %s", len(in.Content), resolvedPath)}, nil
}
