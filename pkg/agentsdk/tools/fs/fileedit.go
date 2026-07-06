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

	newContent, count, diff, errMsg := applyEdit(string(data), in)
	if errMsg != "" {
		return agentsdk.ToolResult{Content: errMsg, IsError: true}, nil
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
	return agentsdk.ToolResult{Content: editSuccessMessage(canonical, count, in.ReplaceAll, diff)}, nil
}

func (t *FileEditTool) Name() string { return "Edit" }

func (t *FileEditTool) Description() string {
	return "Performs exact string replacement in a file. The old_string must match exactly one location (including whitespace and indentation); include enough surrounding lines to make it unique, or set replace_all for intentional multi-site renames. Use Write to create new files; use this for modifying existing ones."
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

	newContent, count, diff, errMsg := applyEdit(string(content), in)
	if errMsg != "" {
		return agentsdk.ToolResult{Content: errMsg, IsError: true}, nil
	}

	resolvedPath, err = resolvePath(workDir, resolvedPath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error resolving file path: %v", err), IsError: true}, nil
	}
	if err := writeFileNoFollow(resolvedPath, []byte(newContent), origMode); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: editSuccessMessage(resolvedPath, count, in.ReplaceAll, diff)}, nil
}

// applyEdit validates the match and applies the requested replacement to
// fileContent. It returns the new content, the number of replacements, and a
// unified diff of the change for the tool result. A non-empty errMsg reports
// a match failure the caller must surface as a tool error.
func applyEdit(fileContent string, in fileEditInput) (newContent string, count int, diff string, errMsg string) {
	count = strings.Count(fileContent, in.OldString)
	if count == 0 {
		return "", 0, "", "old_string not found in file. The match must be byte-exact including whitespace, indentation, and line endings; re-read the relevant lines with read_file and copy them verbatim"
	}
	if count > 1 && !in.ReplaceAll {
		return "", 0, "", fmt.Sprintf("old_string is not unique in file (found %d times). Use replace_all or provide more context to make it unique.", count)
	}
	if in.ReplaceAll {
		newContent = strings.ReplaceAll(fileContent, in.OldString, in.NewString)
	} else {
		newContent = strings.Replace(fileContent, in.OldString, in.NewString, 1)
	}
	return newContent, count, buildEditDiff(fileContent, in.OldString, in.NewString, in.ReplaceAll), ""
}

// editSuccessMessage builds the Edit tool result content: a summary line
// followed by a unified diff of what changed, so the model can verify the
// edit landed and host UIs (e.g. the dashboard) can visualize it.
func editSuccessMessage(path string, count int, replaceAll bool, diff string) string {
	summary := fmt.Sprintf("Successfully edited %s", path)
	if replaceAll {
		summary = fmt.Sprintf("Successfully replaced %d occurrences in %s", count, path)
	}
	if diff == "" {
		return summary
	}
	return summary + "\n" + diff
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
