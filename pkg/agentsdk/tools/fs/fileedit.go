package fs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const maxEditableFileBytes = 5 * 1024 * 1024

// FileEditTool performs exact string replacement in files.
type FileEditTool struct{}

type fileEditInput struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *FileEditTool) resolvePath(workDir, inputPath string) (string, error) {
	return pathutil.Resolve(workDir, inputPath), nil
}

// WorkspaceEditTool restricts edits to the agent workspace.
type WorkspaceEditTool struct {
	FileEditTool
}

func (t *WorkspaceEditTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in fileEditInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.FilePath == "" {
		return agentsdk.ToolResult{Content: "file_path is required", IsError: true}, nil
	}
	if in.OldString == "" {
		return agentsdk.ToolResult{Content: "old_string is required", IsError: true}, nil
	}
	if in.OldString == in.NewString {
		return agentsdk.ToolResult{Content: "old_string and new_string are identical", IsError: true}, nil
	}
	canonical, err := pathutil.ResolveWorkspace(workDir, in.FilePath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}
	// Single open through OpenInWorkspace closes the canonicalize-then-open
	// TOCTOU window on Linux (openat2 RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS).
	rf, err := pathutil.OpenInWorkspace(workDir, canonical, os.O_RDONLY, 0)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}, nil
	}
	info, err := rf.Stat()
	if err != nil {
		_ = rf.Close()
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}, nil
	}
	if !info.Mode().IsRegular() {
		_ = rf.Close()
		return agentsdk.ToolResult{Content: fmt.Sprintf("%s is not a regular file", canonical), IsError: true}, nil
	}
	if info.Size() > maxEditableFileBytes {
		_ = rf.Close()
		return agentsdk.ToolResult{Content: fmt.Sprintf("file is too large to edit (%d bytes, max %d)", info.Size(), maxEditableFileBytes), IsError: true}, nil
	}
	data, err := io.ReadAll(io.LimitReader(rf, maxEditableFileBytes+1))
	_ = rf.Close()
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}, nil
	}
	if len(data) > maxEditableFileBytes {
		return agentsdk.ToolResult{Content: fmt.Sprintf("file is too large to edit (> %d bytes)", maxEditableFileBytes), IsError: true}, nil
	}
	origMode := info.Mode().Perm()

	fileContent := string(data)
	count := strings.Count(fileContent, in.OldString)
	if count == 0 {
		return agentsdk.ToolResult{Content: "old_string not found in file", IsError: true}, nil
	}
	if count > 1 && !in.ReplaceAll {
		return agentsdk.ToolResult{
			Content: fmt.Sprintf("old_string is not unique in file (found %d times). Use replace_all or provide more context to make it unique.", count),
			IsError: true,
		}, nil
	}
	var newContent string
	if in.ReplaceAll {
		newContent = strings.ReplaceAll(fileContent, in.OldString, in.NewString)
	} else {
		newContent = strings.Replace(fileContent, in.OldString, in.NewString, 1)
	}
	wf, err := pathutil.OpenInWorkspace(workDir, canonical, os.O_WRONLY|os.O_TRUNC, origMode)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	if _, err := wf.Write([]byte(newContent)); err != nil {
		_ = wf.Close()
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	if err := wf.Close(); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error closing file: %v", err), IsError: true}, nil
	}
	if in.ReplaceAll {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully replaced %d occurrences in %s", count, canonical)}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully edited %s", canonical)}, nil
}

func (t *FileEditTool) Name() string { return "Edit" }

func (t *FileEditTool) Description() string {
	return "Performs exact string replacement in a file. The old_string must match exactly (including whitespace and indentation)."
}

func (t *FileEditTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "Absolute path to the file to edit"
			},
			"old_string": {
				"type": "string",
				"description": "Exact text to find and replace"
			},
			"new_string": {
				"type": "string",
				"description": "Text to replace old_string with"
			},
			"replace_all": {
				"type": "boolean",
				"description": "Replace all occurrences (default false)",
				"default": false
			}
		},
		"required": ["file_path", "old_string", "new_string"]
	}`)
}

func (t *FileEditTool) IsReadOnly() bool { return false }

func (t *FileEditTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}

func (t *FileEditTool) NeedsApproval() bool { return false }

func (t *FileEditTool) TimeoutSeconds() int { return 0 }

func (t *FileEditTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	return executeFileEdit(ctx, input, workDir, t.resolvePath)
}

func executeFileEdit(ctx context.Context, input json.RawMessage, workDir string, resolvePath func(string, string) (string, error)) (agentsdk.ToolResult, error) {
	var in fileEditInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.FilePath == "" {
		return agentsdk.ToolResult{Content: "file_path is required", IsError: true}, nil
	}
	if in.OldString == "" {
		return agentsdk.ToolResult{Content: "old_string is required", IsError: true}, nil
	}
	if in.OldString == in.NewString {
		return agentsdk.ToolResult{Content: "old_string and new_string are identical", IsError: true}, nil
	}

	resolvedPath, err := resolvePath(workDir, in.FilePath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}

	content, info, err := readEditableFileNoFollow(resolvedPath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}, nil
	}
	origMode := info.Mode()

	fileContent := string(content)
	count := strings.Count(fileContent, in.OldString)
	if count == 0 {
		return agentsdk.ToolResult{Content: "old_string not found in file", IsError: true}, nil
	}
	if count > 1 && !in.ReplaceAll {
		return agentsdk.ToolResult{
			Content: fmt.Sprintf("old_string is not unique in file (found %d times). Use replace_all or provide more context to make it unique.", count),
			IsError: true,
		}, nil
	}

	var newContent string
	if in.ReplaceAll {
		newContent = strings.ReplaceAll(fileContent, in.OldString, in.NewString)
	} else {
		newContent = strings.Replace(fileContent, in.OldString, in.NewString, 1)
	}

	resolvedPath, err = resolvePath(workDir, resolvedPath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}
	if err := writeFileNoFollow(resolvedPath, []byte(newContent), origMode); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	if in.ReplaceAll {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully replaced %d occurrences in %s", count, resolvedPath)}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Successfully edited %s", resolvedPath)}, nil
}

func readEditableFileNoFollow(path string) ([]byte, os.FileInfo, error) {
	f, err := pathutil.OpenFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxEditableFileBytes {
		return nil, nil, fmt.Errorf("file is too large to edit (%d bytes, max %d)", info.Size(), maxEditableFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEditableFileBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(data) > maxEditableFileBytes {
		return nil, nil, fmt.Errorf("file is too large to edit (> %d bytes)", maxEditableFileBytes)
	}
	return data, info, nil
}
