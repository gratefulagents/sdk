package fs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const (
	maxPatchBytes          = 1 * 1024 * 1024
	maxPatchedFileBytes    = 5 * 1024 * 1024
	maxPatchFiles          = 128
	maxPatchAggregateBytes = 64 * 1024 * 1024
	maxPatchPathBytes      = 512
	maxPatchOutputBytes    = 8 * 1024
	maxPatchResultBytes    = 64 * 1024
	maxOpenAIPatchHunks    = 256
	maxOpenAIHunkLines     = 16384
)

type ApplyPatchTool struct{}

type MoveTool struct{}

type DeleteTool struct{}

type applyPatchInput struct {
	Patch  string `json:"patch"`
	DryRun bool   `json:"dry_run"`
}

type moveInput struct {
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
}

type deleteInput struct {
	Path string `json:"path"`
}

type patchResult struct {
	DryRun     bool             `json:"dry_run"`
	Operations []patchOperation `json:"operations"`
	Diff       string           `json:"diff"`
}

type patchOperation struct {
	Operation string `json:"operation"`
	Path      string `json:"path,omitempty"`
	FromPath  string `json:"from_path,omitempty"`
	Mode      string `json:"mode,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
}

type patchFile struct {
	oldPath        string
	newPath        string
	oldPathLiteral bool
	newPathLiteral bool
	sawOldHeader   bool
	sawNewHeader   bool
	sawRenameFrom  bool
	sawRenameTo    bool
	oldMode        *os.FileMode
	newMode        *os.FileMode
	deleteAll      bool
	hunks          []patchHunk
}

type patchHunk struct {
	oldStart  int
	oldCount  int
	newStart  int
	newCount  int
	rangeLess bool
	locator   string
	endOfFile bool
	lines     []patchLine
}

type patchLine struct {
	kind      byte
	text      string
	noNewline bool
}

type workspaceFileState struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

type plannedPatchOperation struct {
	operation string
	oldPath   string
	newPath   string
	data      []byte
	mode      os.FileMode
}

var patchMutationMu sync.Mutex

var unifiedHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)

func (t *ApplyPatchTool) Name() string { return "ApplyPatch" }

func (t *ApplyPatchTool) Description() string {
	return "Applies a unified diff or an OpenAI patch envelope within the workspace. The complete patch is validated before files change; set dry_run to validate and preview without changing files."
}

func (t *ApplyPatchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"patch": {
				"type": "string",
				"description": "Unified diff text, or an OpenAI envelope using *** Begin Patch, *** Update/Add/Delete File, direct change chunks or range-less @@/@@ locator hunks, optional *** Move to, *** End of File, and *** End Patch. Both support file creation, modification, deletion, and rename. Unified diffs also support executable-bit changes."
			},
			"dry_run": {
				"type": "boolean",
				"description": "Validate the complete patch and return its operations without changing files.",
				"default": false
			}
		},
		"required": ["patch"]
	}`)
}

func (t *ApplyPatchTool) IsReadOnly() bool { return false }

func (t *ApplyPatchTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
}

func (t *ApplyPatchTool) NeedsApproval() bool { return false }

func (t *ApplyPatchTool) TimeoutSeconds() int { return 0 }

func (t *ApplyPatchTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in applyPatchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return toolError("Invalid input: %v", err), nil
	}
	files, err := parsePatch(in.Patch)
	if err != nil {
		return toolError("Invalid patch: %v", err), nil
	}
	if !in.DryRun {
		patchMutationMu.Lock()
		defer patchMutationMu.Unlock()
	}
	plans, states, err := planPatch(workDir, files)
	if err != nil {
		return toolError("Patch validation failed: %v", err), nil
	}
	operations := patchAuditOperations(plans)
	result := patchResult{
		DryRun:     in.DryRun,
		Operations: operations,
		Diff:       boundedPatchOutput(in.Patch),
	}
	content, err := json.Marshal(result)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	if len(content) > maxPatchResultBytes {
		return toolError("Patch result is too large (%d bytes, limit %d)", len(content), maxPatchResultBytes), nil
	}
	if !in.DryRun {
		if err := applyPatchPlan(workDir, plans, states); err != nil {
			return toolError("Error applying patch: %v", err), nil
		}
	}
	return agentsdk.ToolResult{Content: string(content)}, nil
}

func (t *MoveTool) Name() string { return "Move" }

func (t *MoveTool) Description() string {
	return "Moves or renames one regular file or directory to a new path within the workspace. The destination must not exist and its parent directory must exist."
}

func (t *MoveTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"source_path": {
				"type": "string",
				"description": "Workspace-relative path of the regular file or directory to move"
			},
			"destination_path": {
				"type": "string",
				"description": "New workspace-relative path. It must not exist and its parent directory must exist."
			}
		},
		"required": ["source_path", "destination_path"]
	}`)
}

func (t *MoveTool) IsReadOnly() bool { return false }

func (t *MoveTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
}

func (t *MoveTool) NeedsApproval() bool { return false }

func (t *MoveTool) TimeoutSeconds() int { return 0 }

func (t *MoveTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in moveInput
	if err := json.Unmarshal(input, &in); err != nil {
		return toolError("Invalid input: %v", err), nil
	}
	source, err := cleanWorkspaceRelative(in.SourcePath)
	if err != nil {
		return toolError("Invalid source_path: %v", err), nil
	}
	destination, err := cleanWorkspaceRelative(in.DestinationPath)
	if err != nil {
		return toolError("Invalid destination_path: %v", err), nil
	}
	if source == destination {
		return toolError("source_path and destination_path must differ"), nil
	}
	if err := pathutil.MovePathInWorkspace(workDir, source, destination); err != nil {
		return toolError("Error moving path: %v", err), nil
	}
	return structuredToolResult(map[string]string{
		"operation":        "move",
		"source_path":      source,
		"destination_path": destination,
	})
}

func (t *DeleteTool) Name() string { return "Delete" }

func (t *DeleteTool) Description() string {
	return "Deletes one regular file or empty directory within the workspace. Delete contained paths explicitly before deleting a non-empty directory."
}

func (t *DeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {
				"type": "string",
				"description": "Workspace-relative path of the regular file or empty directory to delete"
			}
		},
		"required": ["path"]
	}`)
}

func (t *DeleteTool) IsReadOnly() bool { return false }

func (t *DeleteTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
}

func (t *DeleteTool) NeedsApproval() bool { return false }

func (t *DeleteTool) TimeoutSeconds() int { return 0 }

func (t *DeleteTool) Execute(ctx context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in deleteInput
	if err := json.Unmarshal(input, &in); err != nil {
		return toolError("Invalid input: %v", err), nil
	}
	relPath, err := cleanWorkspaceRelative(in.Path)
	if err != nil {
		return toolError("Invalid path: %v", err), nil
	}
	if err := pathutil.DeletePathInWorkspace(workDir, relPath, false); err != nil {
		return toolError("Error deleting path: %v", err), nil
	}
	return structuredToolResult(map[string]string{"operation": "delete", "path": relPath})
}

func toolError(format string, args ...any) agentsdk.ToolResult {
	return agentsdk.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func structuredToolResult(value any) (agentsdk.ToolResult, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	return agentsdk.ToolResult{Content: string(data)}, nil
}

func parsePatch(patch string) ([]patchFile, error) {
	firstLine := patch
	if newline := strings.IndexByte(firstLine, '\n'); newline >= 0 {
		firstLine = firstLine[:newline]
	}
	if strings.TrimSuffix(firstLine, "\r") == "*** Begin Patch" {
		return parseOpenAIPatch(patch)
	}
	return parseUnifiedPatch(patch)
}

func parseOpenAIPatch(patch string) ([]patchFile, error) {
	if strings.TrimSpace(patch) == "" {
		return nil, fmt.Errorf("patch is required")
	}
	if len(patch) > maxPatchBytes {
		return nil, fmt.Errorf("patch is too large (%d bytes, limit %d)", len(patch), maxPatchBytes)
	}
	if strings.IndexByte(patch, 0) >= 0 || !utf8.ValidString(patch) {
		return nil, fmt.Errorf("binary patch data is not supported")
	}

	lines := strings.SplitAfter(patch, "\n")
	if strings.TrimSuffix(strings.TrimSuffix(lines[0], "\n"), "\r") != "*** Begin Patch" {
		return nil, fmt.Errorf("patch must begin with *** Begin Patch")
	}

	var files []patchFile
	var current *patchFile
	kind := ""
	moved := false
	hunkCount := 0
	finish := func() error {
		if current == nil {
			return nil
		}
		for i, hunk := range current.hunks {
			if hunk.endOfFile && i != len(current.hunks)-1 {
				return fmt.Errorf("*** End of File must terminate an update")
			}
		}
		switch kind {
		case "update":
			if len(current.hunks) == 0 && current.oldPath == current.newPath {
				return fmt.Errorf("update file %s contains no changes", current.oldPath)
			}
		case "add":
			if current.newPath == "" {
				return fmt.Errorf("add file has no path")
			}
		case "delete":
			if current.oldPath == "" {
				return fmt.Errorf("delete file has no path")
			}
		}
		files = append(files, *current)
		if len(files) > maxPatchFiles {
			return fmt.Errorf("patch changes too many files (limit %d)", maxPatchFiles)
		}
		current = nil
		kind = ""
		moved = false
		return nil
	}
	appendHunk := func(hunk patchHunk) error {
		if hunkCount >= maxOpenAIPatchHunks {
			return fmt.Errorf("OpenAI patch has too many hunks (limit %d)", maxOpenAIPatchHunks)
		}
		current.hunks = append(current.hunks, hunk)
		hunkCount++
		return nil
	}
	parsePath := func(directive, value string) (string, error) {
		clean, null, err := parsePatchPath(value, false)
		if err != nil {
			return "", fmt.Errorf("%s: %w", directive, err)
		}
		if null {
			return "", fmt.Errorf("%s cannot be /dev/null", directive)
		}
		return clean, nil
	}

	for i := 1; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\n")
		control := strings.TrimSuffix(line, "\r")
		switch {
		case control == "*** End Patch":
			if !hasOnlyTrailingLineEnding(lines, i+1) {
				return nil, fmt.Errorf("content follows *** End Patch")
			}
			if err := finish(); err != nil {
				return nil, err
			}
			if len(files) == 0 {
				return nil, fmt.Errorf("patch contains no file changes")
			}
			return files, nil
		case strings.HasPrefix(control, "*** Update File: "):
			if err := finish(); err != nil {
				return nil, err
			}
			filePath, err := parsePath("update file path", strings.TrimPrefix(control, "*** Update File: "))
			if err != nil {
				return nil, err
			}
			current = &patchFile{oldPath: filePath, newPath: filePath}
			kind = "update"
		case strings.HasPrefix(control, "*** Add File: "):
			if err := finish(); err != nil {
				return nil, err
			}
			filePath, err := parsePath("add file path", strings.TrimPrefix(control, "*** Add File: "))
			if err != nil {
				return nil, err
			}
			current = &patchFile{newPath: filePath}
			kind = "add"
		case strings.HasPrefix(control, "*** Delete File: "):
			if err := finish(); err != nil {
				return nil, err
			}
			filePath, err := parsePath("delete file path", strings.TrimPrefix(control, "*** Delete File: "))
			if err != nil {
				return nil, err
			}
			current = &patchFile{oldPath: filePath, deleteAll: true}
			kind = "delete"
		case strings.HasPrefix(control, "*** Move to: "):
			if current == nil || kind != "update" || moved {
				return nil, fmt.Errorf("move destination must follow one update file directive")
			}
			filePath, err := parsePath("move destination", strings.TrimPrefix(control, "*** Move to: "))
			if err != nil {
				return nil, err
			}
			current.newPath = filePath
			moved = true
		case strings.HasPrefix(control, "@@"):
			if current == nil || kind != "update" {
				return nil, fmt.Errorf("range-less hunk must follow an update file directive")
			}
			hunk, next, err := parseRangeLessPatchHunk(lines, i, strings.TrimSpace(strings.TrimPrefix(control, "@@")))
			if err != nil {
				return nil, err
			}
			if err := appendHunk(hunk); err != nil {
				return nil, err
			}
			i = next - 1
		case control == "*** End of File":
			if current == nil || kind != "update" || len(current.hunks) == 0 {
				return nil, fmt.Errorf("*** End of File must follow an update hunk")
			}
			if current.hunks[len(current.hunks)-1].endOfFile {
				return nil, fmt.Errorf("duplicate *** End of File")
			}
			current.hunks[len(current.hunks)-1].endOfFile = true
		default:
			if current != nil && kind == "update" && len(current.hunks) == 0 && len(line) > 0 && (line[0] == ' ' || line[0] == '+' || line[0] == '-') {
				hunk, next, err := parseRangeLessPatchHunk(lines, i-1, "")
				if err != nil {
					return nil, err
				}
				if err := appendHunk(hunk); err != nil {
					return nil, err
				}
				i = next - 1
				continue
			}
			if current == nil || kind != "add" || len(line) == 0 || line[0] != '+' {
				return nil, fmt.Errorf("unsupported OpenAI patch line %q", control)
			}
			if len(current.hunks) == 0 {
				if err := appendHunk(patchHunk{rangeLess: true}); err != nil {
					return nil, err
				}
			}
			hunk := &current.hunks[0]
			if len(hunk.lines) >= maxOpenAIHunkLines {
				return nil, fmt.Errorf("OpenAI hunk has too many lines (limit %d)", maxOpenAIHunkLines)
			}
			hunk.lines = append(hunk.lines, patchLine{kind: '+', text: line[1:]})
			hunk.newCount++
		}
	}
	return nil, fmt.Errorf("patch is missing *** End Patch")
}

func hasOnlyTrailingLineEnding(lines []string, start int) bool {
	return start == len(lines) || (start+1 == len(lines) && lines[start] == "")
}

func parseRangeLessPatchHunk(lines []string, start int, locator string) (patchHunk, int, error) {
	hunk := patchHunk{rangeLess: true, locator: locator}
	changed := false
	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\n")
		control := strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(control, "@@") || strings.HasPrefix(control, "*** ") {
			if len(hunk.lines) == 0 {
				return patchHunk{}, 0, fmt.Errorf("range-less hunk has no body")
			}
			if !changed {
				return patchHunk{}, 0, fmt.Errorf("range-less hunk has no changes")
			}
			return hunk, i, nil
		}
		if len(line) == 0 || (line[0] != ' ' && line[0] != '+' && line[0] != '-') {
			return patchHunk{}, 0, fmt.Errorf("malformed range-less hunk body")
		}
		if len(hunk.lines) >= maxOpenAIHunkLines {
			return patchHunk{}, 0, fmt.Errorf("OpenAI hunk has too many lines (limit %d)", maxOpenAIHunkLines)
		}
		hunk.lines = append(hunk.lines, patchLine{kind: line[0], text: line[1:]})
		switch line[0] {
		case ' ':
			hunk.oldCount++
			hunk.newCount++
		case '-':
			hunk.oldCount++
			changed = true
		case '+':
			hunk.newCount++
			changed = true
		}
	}
	return patchHunk{}, 0, fmt.Errorf("range-less hunk is not followed by a patch directive")
}

func parseUnifiedPatch(patch string) ([]patchFile, error) {
	if strings.TrimSpace(patch) == "" {
		return nil, fmt.Errorf("patch is required")
	}
	if len(patch) > maxPatchBytes {
		return nil, fmt.Errorf("patch is too large (%d bytes, limit %d)", len(patch), maxPatchBytes)
	}
	if strings.IndexByte(patch, 0) >= 0 || !utf8.ValidString(patch) {
		return nil, fmt.Errorf("binary patch data is not supported")
	}

	lines := strings.SplitAfter(patch, "\n")
	var files []patchFile
	var current *patchFile
	var diffOld, diffNew string
	var haveDiffPaths bool
	finish := func() error {
		if current == nil {
			return nil
		}
		if current.oldPath == "" && current.newPath == "" && !haveDiffPaths {
			return fmt.Errorf("patch file has no paths")
		}
		if current.oldPath == "" && haveDiffPaths {
			current.oldPath = diffOld
		}
		if current.newPath == "" && haveDiffPaths {
			current.newPath = diffNew
		}
		if len(current.hunks) > 0 && (!current.sawOldHeader || !current.sawNewHeader) {
			return fmt.Errorf("patch hunks require exactly one ---/+++ header pair")
		}
		if current.sawRenameFrom != current.sawRenameTo {
			return fmt.Errorf("rename requires paired rename from/to directives")
		}
		oldPath, oldNull, err := parsePatchPath(current.oldPath, !current.oldPathLiteral)
		if err != nil {
			return fmt.Errorf("old path: %w", err)
		}
		newPath, newNull, err := parsePatchPath(current.newPath, !current.newPathLiteral)
		if err != nil {
			return fmt.Errorf("new path: %w", err)
		}
		if oldNull && newNull {
			return fmt.Errorf("patch file cannot have /dev/null as both paths")
		}
		if oldNull {
			current.oldPath = ""
		} else {
			current.oldPath = oldPath
		}
		if newNull {
			current.newPath = ""
		} else {
			current.newPath = newPath
		}
		if haveDiffPaths {
			diffOldPath, diffOldNull, err := parsePatchPath(diffOld, true)
			if err != nil {
				return fmt.Errorf("diff old path: %w", err)
			}
			diffNewPath, diffNewNull, err := parsePatchPath(diffNew, true)
			if err != nil {
				return fmt.Errorf("diff new path: %w", err)
			}
			if (current.oldPath != "" && !diffOldNull && current.oldPath != diffOldPath) || (current.newPath != "" && !diffNewNull && current.newPath != diffNewPath) {
				return fmt.Errorf("patch headers or rename metadata disagree with diff paths")
			}
		}
		if current.oldPath != "" && current.newPath != "" && current.oldPath != current.newPath && !(current.sawRenameFrom && current.sawRenameTo) {
			return fmt.Errorf("rename from %s to %s requires paired rename metadata", current.oldPath, current.newPath)
		}
		if current.oldPath == current.newPath && len(current.hunks) == 0 && current.oldMode == nil && current.newMode == nil {
			return fmt.Errorf("patch file %s contains no changes", current.oldPath)
		}
		if current.oldPath == "" && current.oldMode != nil {
			return fmt.Errorf("new file %s cannot declare an old mode", current.newPath)
		}
		if current.newPath == "" && current.newMode != nil {
			return fmt.Errorf("deleted file %s cannot declare a new mode", current.oldPath)
		}
		files = append(files, *current)
		if len(files) > maxPatchFiles {
			return fmt.Errorf("patch changes too many files (limit %d)", maxPatchFiles)
		}
		current = nil
		diffOld, diffNew, haveDiffPaths = "", "", false
		return nil
	}

	for i := 0; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\n")
		control := strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(control, "GIT binary patch") || strings.HasPrefix(control, "Binary files ") {
			return nil, fmt.Errorf("binary patches are not supported")
		}
		switch {
		case strings.HasPrefix(control, "diff --git "):
			if err := finish(); err != nil {
				return nil, err
			}
			parts := strings.Fields(control)
			if len(parts) != 4 {
				return nil, fmt.Errorf("quoted or malformed diff paths are not supported")
			}
			current = &patchFile{}
			diffOld, diffNew, haveDiffPaths = parts[2], parts[3], true
		case strings.HasPrefix(control, "--- "):
			if current != nil && current.sawOldHeader {
				if current.sawNewHeader && len(current.hunks) > 0 && !haveDiffPaths {
					if err := finish(); err != nil {
						return nil, err
					}
				} else {
					return nil, fmt.Errorf("duplicate old-file header")
				}
			}
			if current == nil {
				current = &patchFile{}
			}
			current.oldPath = patchHeaderPath(strings.TrimPrefix(control, "--- "))
			current.sawOldHeader = true
		case strings.HasPrefix(control, "+++ "):
			if current == nil || !current.sawOldHeader {
				return nil, fmt.Errorf("new path appears before old path")
			}
			if current.sawNewHeader {
				return nil, fmt.Errorf("duplicate new-file header")
			}
			current.newPath = patchHeaderPath(strings.TrimPrefix(control, "+++ "))
			current.sawNewHeader = true
		case strings.HasPrefix(control, "old mode "):
			if current == nil {
				return nil, fmt.Errorf("old mode appears before a file path")
			}
			if current.oldMode != nil {
				return nil, fmt.Errorf("duplicate old mode")
			}
			mode, err := parsePatchMode(strings.TrimPrefix(control, "old mode "))
			if err != nil {
				return nil, err
			}
			current.oldMode = &mode
		case strings.HasPrefix(control, "new mode "):
			if current == nil {
				return nil, fmt.Errorf("new mode appears before a file path")
			}
			if current.newMode != nil {
				return nil, fmt.Errorf("duplicate new mode")
			}
			mode, err := parsePatchMode(strings.TrimPrefix(control, "new mode "))
			if err != nil {
				return nil, err
			}
			current.newMode = &mode
		case strings.HasPrefix(control, "new file mode "):
			if current == nil {
				return nil, fmt.Errorf("new file mode appears before a file path")
			}
			if current.newMode != nil {
				return nil, fmt.Errorf("duplicate new mode")
			}
			mode, err := parsePatchMode(strings.TrimPrefix(control, "new file mode "))
			if err != nil {
				return nil, err
			}
			current.newMode = &mode
		case strings.HasPrefix(control, "deleted file mode "):
			if current == nil {
				return nil, fmt.Errorf("deleted file mode appears before a file path")
			}
			if current.oldMode != nil {
				return nil, fmt.Errorf("duplicate old mode")
			}
			mode, err := parsePatchMode(strings.TrimPrefix(control, "deleted file mode "))
			if err != nil {
				return nil, err
			}
			current.oldMode = &mode
		case strings.HasPrefix(control, "rename from "):
			if current == nil {
				return nil, fmt.Errorf("rename source appears before a file path")
			}
			if current.sawRenameFrom {
				return nil, fmt.Errorf("duplicate rename source")
			}
			current.oldPath = strings.TrimPrefix(control, "rename from ")
			current.oldPathLiteral = true
			current.sawRenameFrom = true
		case strings.HasPrefix(control, "rename to "):
			if current == nil {
				return nil, fmt.Errorf("rename destination appears before a file path")
			}
			if current.sawRenameTo {
				return nil, fmt.Errorf("duplicate rename destination")
			}
			current.newPath = strings.TrimPrefix(control, "rename to ")
			current.newPathLiteral = true
			current.sawRenameTo = true
		case strings.HasPrefix(control, "copy from "), strings.HasPrefix(control, "copy to "):
			return nil, fmt.Errorf("copy directives are not supported")
		case strings.HasPrefix(control, "@@ "):
			if current == nil {
				return nil, fmt.Errorf("hunk appears before a file path")
			}
			hunk, next, err := parsePatchHunk(lines, i)
			if err != nil {
				return nil, err
			}
			current.hunks = append(current.hunks, hunk)
			i = next - 1
		case strings.HasPrefix(control, "index "), strings.HasPrefix(control, "similarity index "), strings.HasPrefix(control, "dissimilarity index "):
			// Non-semantic Git metadata is accepted but does not affect application.
		case control == "":
			// Allow a trailing blank line between file sections.
		default:
			return nil, fmt.Errorf("unsupported or out-of-hunk patch line %q", control)
		}
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch contains no file changes")
	}
	return files, nil
}

func patchHeaderPath(value string) string {
	if tab := strings.IndexByte(value, '\t'); tab >= 0 {
		return value[:tab]
	}
	return value
}

func parsePatchPath(value string, stripDiffPrefix bool) (string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "/dev/null" {
		return "", true, nil
	}
	if stripDiffPrefix && (strings.HasPrefix(value, "a/") || strings.HasPrefix(value, "b/")) {
		value = value[2:]
	}
	clean, err := cleanWorkspaceRelative(value)
	return clean, false, err
}

func cleanWorkspaceRelative(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is required")
	}
	if len(value) > maxPatchPathBytes {
		return "", fmt.Errorf("path is too long (%d bytes, limit %d)", len(value), maxPatchPathBytes)
	}
	if strings.ContainsAny(value, "\\\x00") || strings.ContainsAny(value, "\t\n\r") || path.IsAbs(value) {
		return "", fmt.Errorf("path must be a relative slash-separated path")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "." || component == ".." {
			return "", fmt.Errorf("path traversal is not allowed")
		}
	}
	clean := path.Clean(value)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return "", fmt.Errorf("path traversal is not allowed")
	}
	return clean, nil
}

func parsePatchMode(value string) (os.FileMode, error) {
	switch value {
	case "100644":
		return 0o644, nil
	case "100755":
		return 0o755, nil
	default:
		return 0, fmt.Errorf("unsupported Git blob mode %q (only 100644 and 100755 are supported)", value)
	}
}

func parsePatchHunk(lines []string, start int) (patchHunk, int, error) {
	line := strings.TrimSuffix(strings.TrimSuffix(lines[start], "\n"), "\r")
	match := unifiedHunkHeader.FindStringSubmatch(line)
	if match == nil {
		return patchHunk{}, 0, fmt.Errorf("malformed hunk header %q", line)
	}
	oldStart, err := strconv.Atoi(match[1])
	if err != nil {
		return patchHunk{}, 0, err
	}
	oldCount := 1
	if match[2] != "" {
		oldCount, err = strconv.Atoi(match[2])
		if err != nil {
			return patchHunk{}, 0, err
		}
	}
	newStart, err := strconv.Atoi(match[3])
	if err != nil {
		return patchHunk{}, 0, err
	}
	newCount := 1
	if match[4] != "" {
		newCount, err = strconv.Atoi(match[4])
		if err != nil {
			return patchHunk{}, 0, err
		}
	}
	if oldStart < 0 || newStart < 0 || (oldCount > 0 && oldStart == 0) || (newCount > 0 && newStart == 0) {
		return patchHunk{}, 0, fmt.Errorf("invalid hunk line range")
	}
	hunk := patchHunk{oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
	oldSeen, newSeen := 0, 0
	for i := start + 1; ; i++ {
		if oldSeen == oldCount && newSeen == newCount {
			for i < len(lines) {
				marker := strings.TrimSuffix(strings.TrimSuffix(lines[i], "\n"), "\r")
				if marker != "\\ No newline at end of file" {
					return hunk, i, nil
				}
				if len(hunk.lines) == 0 {
					return patchHunk{}, 0, fmt.Errorf("newline marker without a hunk line")
				}
				hunk.lines[len(hunk.lines)-1].noNewline = true
				i++
			}
			return hunk, i, nil
		}
		if i >= len(lines) {
			return patchHunk{}, 0, fmt.Errorf("hunk body is shorter than its header")
		}
		body := strings.TrimSuffix(lines[i], "\n")
		control := strings.TrimSuffix(body, "\r")
		if control == "\\ No newline at end of file" {
			if len(hunk.lines) == 0 {
				return patchHunk{}, 0, fmt.Errorf("newline marker without a hunk line")
			}
			hunk.lines[len(hunk.lines)-1].noNewline = true
			continue
		}
		if len(body) == 0 || (body[0] != ' ' && body[0] != '+' && body[0] != '-') {
			return patchHunk{}, 0, fmt.Errorf("malformed hunk body")
		}
		line := patchLine{kind: body[0], text: body[1:]}
		switch line.kind {
		case ' ':
			oldSeen++
			newSeen++
		case '-':
			oldSeen++
		case '+':
			newSeen++
		}
		if oldSeen > oldCount || newSeen > newCount {
			return patchHunk{}, 0, fmt.Errorf("hunk body is longer than its header")
		}
		hunk.lines = append(hunk.lines, line)
	}
}

func planPatch(workDir string, files []patchFile) ([]plannedPatchOperation, map[string]workspaceFileState, error) {
	sourcePaths := make(map[string]bool)
	targetPaths := make(map[string]bool)
	allPaths := make(map[string]bool)
	for _, file := range files {
		if file.oldPath != "" {
			if sourcePaths[file.oldPath] {
				return nil, nil, fmt.Errorf("multiple patch operations use source %s", file.oldPath)
			}
			sourcePaths[file.oldPath] = true
			allPaths[file.oldPath] = true
		}
		if file.newPath != "" {
			if file.newPath != file.oldPath {
				if targetPaths[file.newPath] {
					return nil, nil, fmt.Errorf("multiple patch operations use destination %s", file.newPath)
				}
				targetPaths[file.newPath] = true
			}
			allPaths[file.newPath] = true
		}
	}
	for destination := range targetPaths {
		if sourcePaths[destination] {
			return nil, nil, fmt.Errorf("conflicting patch paths include %s", destination)
		}
	}
	finalPaths := make([]string, 0, len(files))
	for _, file := range files {
		if file.newPath != "" {
			for _, existing := range finalPaths {
				if strings.HasPrefix(file.newPath, existing+"/") || strings.HasPrefix(existing, file.newPath+"/") {
					return nil, nil, fmt.Errorf("conflicting ancestor patch paths %s and %s", existing, file.newPath)
				}
			}
			finalPaths = append(finalPaths, file.newPath)
		}
	}

	states := make(map[string]workspaceFileState, len(allPaths))
	totalBytes := 0
	for relPath := range allPaths {
		state, err := inspectWorkspaceFile(workDir, relPath)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", relPath, err)
		}
		totalBytes += len(state.data)
		if totalBytes > maxPatchAggregateBytes {
			return nil, nil, fmt.Errorf("patch source data exceeds aggregate limit of %d bytes", maxPatchAggregateBytes)
		}
		states[relPath] = state
	}

	plans := make([]plannedPatchOperation, 0, len(files))
	for _, file := range files {
		var operation string
		var source workspaceFileState
		switch {
		case file.oldPath == "":
			operation = "create"
			if states[file.newPath].exists {
				return nil, nil, fmt.Errorf("create destination %s already exists", file.newPath)
			}
		case file.newPath == "":
			operation = "delete"
			source = states[file.oldPath]
			if !source.exists {
				return nil, nil, fmt.Errorf("delete source %s does not exist", file.oldPath)
			}
		case file.oldPath == file.newPath:
			operation = "modify"
			source = states[file.oldPath]
			if !source.exists {
				return nil, nil, fmt.Errorf("modify source %s does not exist", file.oldPath)
			}
		case file.oldPath != file.newPath:
			operation = "rename"
			source = states[file.oldPath]
			if !source.exists {
				return nil, nil, fmt.Errorf("rename source %s does not exist", file.oldPath)
			}
			if states[file.newPath].exists {
				return nil, nil, fmt.Errorf("rename destination %s already exists", file.newPath)
			}
		}
		if file.oldMode != nil && file.oldPath != "" && executableBits(source.mode) != executableBits(*file.oldMode) {
			return nil, nil, fmt.Errorf("mode conflict for %s", file.oldPath)
		}
		if operation == "create" && file.oldMode != nil {
			return nil, nil, fmt.Errorf("create operation has an old mode")
		}
		if operation == "delete" && file.newMode != nil {
			return nil, nil, fmt.Errorf("delete operation has a new mode")
		}

		original := ""
		if source.exists {
			original = string(source.data)
		}
		updated := ""
		if !file.deleteAll {
			var err error
			updated, err = applyPatchHunks(original, file.hunks)
			if err != nil {
				pathForError := file.oldPath
				if pathForError == "" {
					pathForError = file.newPath
				}
				return nil, nil, fmt.Errorf("%s: %w", pathForError, err)
			}
		}
		if operation == "delete" && updated != "" {
			return nil, nil, fmt.Errorf("delete patch for %s does not remove all file content", file.oldPath)
		}
		if len(updated) > maxPatchedFileBytes {
			return nil, nil, fmt.Errorf("patched file %s is too large (%d bytes, limit %d)", file.newPath, len(updated), maxPatchedFileBytes)
		}
		mode := source.mode.Perm()
		if operation == "create" {
			mode = 0o644
		}
		if file.newMode != nil {
			if operation == "create" {
				mode = file.newMode.Perm()
			} else if operation != "delete" {
				mode = mode&^0o111 | executableBits(*file.newMode)
			}
		}
		plans = append(plans, plannedPatchOperation{
			operation: operation,
			oldPath:   file.oldPath,
			newPath:   file.newPath,
			data:      []byte(updated),
			mode:      mode,
		})
	}
	return plans, states, nil
}

func inspectWorkspaceFile(workDir, relPath string) (workspaceFileState, error) {
	file, err := pathutil.OpenInWorkspace(workDir, filepath.FromSlash(relPath), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return workspaceFileState{}, nil
		}
		return workspaceFileState{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return workspaceFileState{}, err
	}
	if !info.Mode().IsRegular() {
		return workspaceFileState{}, fmt.Errorf("is not a regular file")
	}
	if err := pathutil.RequireSingleLink(info); err != nil {
		return workspaceFileState{}, err
	}
	if info.Size() > maxPatchedFileBytes {
		return workspaceFileState{}, fmt.Errorf("is too large (%d bytes, limit %d)", info.Size(), maxPatchedFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPatchedFileBytes+1))
	if err != nil {
		return workspaceFileState{}, err
	}
	if len(data) > maxPatchedFileBytes {
		return workspaceFileState{}, fmt.Errorf("is too large (limit %d)", maxPatchedFileBytes)
	}
	if strings.IndexByte(string(data), 0) >= 0 || !utf8.Valid(data) {
		return workspaceFileState{}, fmt.Errorf("is a binary file")
	}
	return workspaceFileState{exists: true, data: data, mode: info.Mode().Perm()}, nil
}

func applyPatchHunks(content string, hunks []patchHunk) (string, error) {
	if len(hunks) == 0 {
		return content, nil
	}
	if hunks[0].rangeLess {
		return applyRangeLessPatchHunks(content, hunks)
	}
	starts := patchLineStarts(content)
	lineCount := len(starts)
	if content == "" {
		lineCount = 0
	}
	cursor := 0
	lineDelta := 0
	var result strings.Builder
	for _, hunk := range hunks {
		position := hunk.oldStart
		if hunk.oldCount > 0 {
			position--
			if position < 0 || position+hunk.oldCount > lineCount {
				return "", fmt.Errorf("hunk line range does not exist")
			}
		} else if position < 0 || position > lineCount {
			return "", fmt.Errorf("hunk insertion line range does not exist")
		}
		expectedNewStart := position + lineDelta
		if hunk.newCount > 0 {
			expectedNewStart++
		}
		if hunk.newStart != expectedNewStart {
			return "", fmt.Errorf("hunk new-file range is inconsistent: got %d, want %d", hunk.newStart, expectedNewStart)
		}
		start := len(content)
		if position < lineCount {
			start = starts[position]
		}
		oldText, newText := hunkText(hunk)
		if start < cursor {
			return "", fmt.Errorf("overlapping hunks")
		}
		if len(oldText) > len(content)-start || content[start:start+len(oldText)] != oldText {
			return "", fmt.Errorf("hunk does not match file content")
		}
		result.WriteString(content[cursor:start])
		result.WriteString(newText)
		cursor = start + len(oldText)
		lineDelta += hunk.newCount - hunk.oldCount
	}
	result.WriteString(content[cursor:])
	if result.Len() > maxPatchedFileBytes {
		return "", fmt.Errorf("patched file is too large (%d bytes, limit %d)", result.Len(), maxPatchedFileBytes)
	}
	return result.String(), nil
}

func applyRangeLessPatchHunks(content string, hunks []patchHunk) (string, error) {
	edits := make([]rangeLessEdit, 0)
	hunkCursor := 0
	changedCursor := 0
	for _, hunk := range hunks {
		if !hunk.rangeLess {
			return "", fmt.Errorf("cannot mix range-less and unified hunks")
		}
		oldLines, _ := rangeLessHunkLines(hunk)
		if len(oldLines) == 0 {
			if content != "" || len(hunks) != 1 {
				return "", fmt.Errorf("range-less hunk has no context or removed lines")
			}
			_, newLines := rangeLessHunkLines(hunk)
			edits = append(edits, rangeLessEdit{newLines: newLines})
			continue
		}

		searchStart := hunkCursor
		if hunk.locator != "" {
			var found bool
			searchStart, found = rangeLessLocatorEnd(content, hunkCursor, hunk.locator)
			if !found {
				return "", fmt.Errorf("range-less hunk locator does not match file content")
			}
		}

		match, ambiguous := rangeLessLineSequenceMatch(content, searchStart, oldLines, hunk.endOfFile)
		if ambiguous {
			return "", fmt.Errorf("range-less hunk matches file content more than once")
		}
		if match < 0 {
			return "", fmt.Errorf("range-less hunk does not match file content")
		}
		for _, edit := range rangeLessHunkEdits(hunk, content, match) {
			if edit.start < changedCursor {
				return "", fmt.Errorf("overlapping range-less hunks")
			}
			edits = append(edits, edit)
			changedCursor = edit.end
		}
		hunkCursor = match
	}

	var result strings.Builder
	cursor := 0
	for _, edit := range edits {
		result.WriteString(content[cursor:edit.start])
		result.WriteString(strings.Join(edit.newLines, ""))
		cursor = edit.end
	}
	result.WriteString(content[cursor:])
	if result.Len() > maxPatchedFileBytes {
		return "", fmt.Errorf("patched file is too large (%d bytes, limit %d)", result.Len(), maxPatchedFileBytes)
	}
	return result.String(), nil
}

type rangeLessEdit struct {
	start    int
	end      int
	newLines []string
}

type rangeLessPatternLine struct {
	text string
	hash uint64
}

func rangeLessHunkLines(hunk patchHunk) ([]string, []string) {
	oldLines := make([]string, 0, hunk.oldCount)
	newLines := make([]string, 0, hunk.newCount)
	for _, line := range hunk.lines {
		text := line.text
		if !line.noNewline {
			text += "\n"
		}
		switch line.kind {
		case ' ':
			oldLines = append(oldLines, text)
			newLines = append(newLines, text)
		case '-':
			oldLines = append(oldLines, text)
		case '+':
			newLines = append(newLines, text)
		}
	}
	return oldLines, newLines
}

func rangeLessHunkEdits(hunk patchHunk, content string, start int) []rangeLessEdit {
	edits := make([]rangeLessEdit, 0)
	position := start
	for i := 0; i < len(hunk.lines); {
		if hunk.lines[i].kind == ' ' {
			position = rangeLessNextLineEnd(content, position)
			i++
			continue
		}
		edit := rangeLessEdit{start: position, end: position}
		for i < len(hunk.lines) && hunk.lines[i].kind != ' ' {
			patchLine := hunk.lines[i]
			text := patchLine.text
			if !patchLine.noNewline {
				text += "\n"
			}
			if patchLine.kind == '-' {
				edit.end = rangeLessNextLineEnd(content, edit.end)
				position = edit.end
			} else {
				edit.newLines = append(edit.newLines, text)
			}
			i++
		}
		edits = append(edits, edit)
	}
	return edits
}

func rangeLessLocatorEnd(content string, start int, locator string) (int, bool) {
	for start < len(content) {
		end := rangeLessNextLineEnd(content, start)
		if patchContentLineText(content[start:end]) == locator {
			return end, true
		}
		start = end
	}
	return 0, false
}

func rangeLessLineSequenceMatch(content string, start int, expected []string, endOfFile bool) (int, bool) {
	pattern := make([]rangeLessPatternLine, len(expected))
	for i, text := range expected {
		pattern[i] = rangeLessPatternLine{text: text, hash: rangeLessLineHash(text)}
	}
	prefix := rangeLessKMPPrefix(pattern)
	starts := make([]int, len(pattern))
	match := -1
	matched := 0
	line := 0
	for start < len(content) {
		end := rangeLessNextLineEnd(content, start)
		hash := rangeLessLineHash(content[start:end])
		for matched > 0 && !rangeLessLineEqual(content, start, end, hash, pattern[matched]) {
			matched = prefix[matched-1]
		}
		if rangeLessLineEqual(content, start, end, hash, pattern[matched]) {
			matched++
		}
		starts[line%len(pattern)] = start
		line++
		if matched == len(pattern) {
			if !endOfFile || end == len(content) {
				if match >= 0 {
					return 0, true
				}
				match = starts[(line-len(pattern))%len(pattern)]
			}
			matched = prefix[matched-1]
		}
		start = end
	}
	return match, false
}

func rangeLessKMPPrefix(pattern []rangeLessPatternLine) []int {
	prefix := make([]int, len(pattern))
	for i, matched := 1, 0; i < len(pattern); i++ {
		for matched > 0 && !rangeLessPatternLineEqual(pattern[i], pattern[matched]) {
			matched = prefix[matched-1]
		}
		if rangeLessPatternLineEqual(pattern[i], pattern[matched]) {
			matched++
		}
		prefix[i] = matched
	}
	return prefix
}

func rangeLessLineEqual(content string, start, end int, hash uint64, expected rangeLessPatternLine) bool {
	return hash == expected.hash && content[start:end] == expected.text
}

func rangeLessPatternLineEqual(left, right rangeLessPatternLine) bool {
	return left.hash == right.hash && left.text == right.text
}

func rangeLessLineHash(line string) uint64 {
	const offset = 14695981039346656037
	const prime = 1099511628211
	hash := uint64(offset)
	for i := range len(line) {
		hash ^= uint64(line[i])
		hash *= prime
	}
	return hash
}

func rangeLessNextLineEnd(content string, start int) int {
	if newline := strings.IndexByte(content[start:], '\n'); newline >= 0 {
		return start + newline + 1
	}
	return len(content)
}

func patchContentLineText(line string) string {
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

func patchLineStarts(content string) []int {
	starts := []int{0}
	for i := 0; i+1 < len(content); i++ {
		if content[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func hunkText(hunk patchHunk) (string, string) {
	var oldText, newText strings.Builder
	for _, line := range hunk.lines {
		appendLine := func(builder *strings.Builder) {
			builder.WriteString(line.text)
			if !line.noNewline {
				builder.WriteByte('\n')
			}
		}
		switch line.kind {
		case ' ':
			appendLine(&oldText)
			appendLine(&newText)
		case '-':
			appendLine(&oldText)
		case '+':
			appendLine(&newText)
		}
	}
	return oldText.String(), newText.String()
}

func executableBits(mode os.FileMode) os.FileMode {
	return mode.Perm() & 0o111
}

func applyPatchPlan(workDir string, plans []plannedPatchOperation, states map[string]workspaceFileState) error {
	for relPath, expected := range states {
		current, err := inspectWorkspaceFile(workDir, relPath)
		if err != nil {
			return err
		}
		if !workspaceStatesEqual(current, expected) {
			return fmt.Errorf("file changed during patch validation: %s", relPath)
		}
	}
	for _, plan := range plans {
		if plan.operation == "delete" {
			continue
		}
		parent := filepath.Dir(filepath.FromSlash(plan.newPath))
		directory, err := pathutil.OpenInWorkspace(workDir, parent, os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("destination parent for %s: %w", plan.newPath, err)
		}
		info, statErr := directory.Stat()
		directory.Close()
		if statErr != nil || !info.IsDir() {
			return fmt.Errorf("destination parent for %s is not a directory", plan.newPath)
		}
	}
	mutated := make(map[string]bool)
	finals := make(map[string]workspaceFileState)
	for _, plan := range plans {
		if plan.newPath != "" {
			finals[plan.newPath] = workspaceFileState{exists: true, data: plan.data, mode: plan.mode.Perm()}
		}
		if plan.oldPath != "" && plan.oldPath != plan.newPath {
			finals[plan.oldPath] = workspaceFileState{}
		}
	}
	rollback := func(applyErr error) error {
		if len(mutated) == 0 {
			return applyErr
		}
		if rollbackErr := rollbackPatchPlan(workDir, states, finals, mutated); rollbackErr != nil {
			return fmt.Errorf("%v; rollback failed safely without overwriting concurrent changes: %w", applyErr, rollbackErr)
		}
		return applyErr
	}
	for _, plan := range plans {
		if plan.operation == "delete" {
			continue
		}
		var err error
		if plan.operation == "create" || plan.operation == "rename" {
			err = pathutil.CreateExclusiveFileInWorkspace(workDir, filepath.FromSlash(plan.newPath), plan.data, plan.mode)
		} else {
			err = replacePatchFileIfUnchanged(workDir, plan.newPath, states[plan.newPath], plan.data, plan.mode)
		}
		if err != nil {
			return rollback(err)
		}
		mutated[plan.newPath] = true
	}
	for _, plan := range plans {
		if plan.operation != "delete" && plan.operation != "rename" {
			continue
		}
		if err := deletePatchFileIfUnchanged(workDir, plan.oldPath, states[plan.oldPath]); err != nil {
			return rollback(err)
		}
		mutated[plan.oldPath] = true
	}
	return nil
}

func replacePatchFileIfUnchanged(workDir, relPath string, expected workspaceFileState, data []byte, mode os.FileMode) error {
	quarantine, err := quarantinePatchFile(workDir, relPath, expected)
	if err != nil {
		return err
	}
	restore := func(cause error) error {
		if err := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(quarantine), filepath.FromSlash(relPath)); err != nil {
			return fmt.Errorf("%v; restoring quarantined source failed: %w", cause, err)
		}
		return cause
	}
	if err := pathutil.CreateExclusiveFileInWorkspace(workDir, filepath.FromSlash(relPath), data, mode); err != nil {
		return restore(err)
	}
	if err := pathutil.DeleteFileInWorkspace(workDir, filepath.FromSlash(quarantine)); err != nil {
		created := workspaceFileState{exists: true, data: data, mode: mode.Perm()}
		createdQuarantine, claimErr := quarantinePatchFile(workDir, relPath, created)
		if claimErr != nil {
			return fmt.Errorf("deleting source quarantine failed: %v; refusing to remove unowned destination: %w", err, claimErr)
		}
		cleanupErr := pathutil.DeleteFileInWorkspace(workDir, filepath.FromSlash(createdQuarantine))
		restoreErr := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(quarantine), filepath.FromSlash(relPath))
		if cleanupErr != nil || restoreErr != nil {
			return fmt.Errorf("deleting quarantine failed: %v; cleanup failed: %v; restore failed: %v", err, cleanupErr, restoreErr)
		}
		return err
	}
	return nil
}

func deletePatchFileIfUnchanged(workDir, relPath string, expected workspaceFileState) error {
	quarantine, err := quarantinePatchFile(workDir, relPath, expected)
	if err != nil {
		return err
	}
	if err := pathutil.DeleteFileInWorkspace(workDir, filepath.FromSlash(quarantine)); err != nil {
		if restoreErr := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(quarantine), filepath.FromSlash(relPath)); restoreErr != nil {
			return fmt.Errorf("deleting quarantine failed: %v; restoring source failed: %w", err, restoreErr)
		}
		return err
	}
	return nil
}

func quarantinePatchFile(workDir, relPath string, expected workspaceFileState) (string, error) {
	parent := path.Dir(relPath)
	for range 100 {
		var random [12]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", err
		}
		name := ".agentsdk-patch-" + hex.EncodeToString(random[:])
		quarantine := name
		if parent != "." {
			quarantine = path.Join(parent, name)
		}
		err := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(relPath), filepath.FromSlash(quarantine))
		if err != nil {
			if strings.Contains(err.Error(), "exists") {
				continue
			}
			return "", err
		}
		actual, err := inspectWorkspaceFile(workDir, quarantine)
		if err == nil && workspaceStatesEqual(actual, expected) {
			return quarantine, nil
		}
		if restoreErr := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(quarantine), filepath.FromSlash(relPath)); restoreErr != nil {
			return "", fmt.Errorf("source changed during patch and restoring quarantine failed: %w", restoreErr)
		}
		if err != nil {
			return "", err
		}
		return "", fmt.Errorf("file changed during patch validation: %s", relPath)
	}
	return "", fmt.Errorf("could not allocate patch quarantine path")
}

func rollbackPatchPlan(workDir string, states, finals map[string]workspaceFileState, mutated map[string]bool) error {
	for relPath := range mutated {
		original := states[relPath]
		final := finals[relPath]
		if !final.exists {
			if !original.exists {
				continue
			}
			if err := pathutil.CreateExclusiveFileInWorkspace(workDir, filepath.FromSlash(relPath), original.data, original.mode); err != nil {
				return fmt.Errorf("restoring deleted path %s without clobbering concurrent work: %w", relPath, err)
			}
			continue
		}
		quarantine, err := quarantinePatchFile(workDir, relPath, final)
		if err != nil {
			return fmt.Errorf("claiming patch-owned path %s for rollback: %w", relPath, err)
		}
		if original.exists {
			if err := pathutil.CreateExclusiveFileInWorkspace(workDir, filepath.FromSlash(relPath), original.data, original.mode); err != nil {
				restoreErr := pathutil.MoveFileInWorkspace(workDir, filepath.FromSlash(quarantine), filepath.FromSlash(relPath))
				return fmt.Errorf("restoring %s: %v; patch output recovery at %s failed: %v", relPath, err, quarantine, restoreErr)
			}
		}
		if err := pathutil.DeleteFileInWorkspace(workDir, filepath.FromSlash(quarantine)); err != nil {
			return fmt.Errorf("deleting rolled-back patch output %s: %w", quarantine, err)
		}
	}
	return nil
}

func workspaceStatesEqual(left, right workspaceFileState) bool {
	return left.exists == right.exists && (!left.exists || (left.mode == right.mode && string(left.data) == string(right.data)))
}

func patchAuditOperations(plans []plannedPatchOperation) []patchOperation {
	operations := make([]patchOperation, 0, len(plans))
	for _, plan := range plans {
		op := patchOperation{Operation: plan.operation}
		switch plan.operation {
		case "rename":
			op.FromPath = plan.oldPath
			op.Path = plan.newPath
			op.Bytes = len(plan.data)
			op.Mode = fmt.Sprintf("%04o", plan.mode.Perm())
		case "delete":
			op.Path = plan.oldPath
		default:
			op.Path = plan.newPath
			op.Bytes = len(plan.data)
			op.Mode = fmt.Sprintf("%04o", plan.mode.Perm())
		}
		operations = append(operations, op)
	}
	return operations
}

func boundedPatchOutput(patch string) string {
	if len(patch) <= maxPatchOutputBytes {
		return patch
	}
	cut := strings.LastIndexByte(patch[:maxPatchOutputBytes], '\n')
	if cut <= 0 {
		cut = maxPatchOutputBytes
	}
	return patch[:cut] + "\n... [diff truncated]"
}
