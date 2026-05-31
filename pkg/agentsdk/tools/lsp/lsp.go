package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

// Tool provides Language Server Protocol operations via gopls.
type Tool struct{}

var runSandboxCommand = func(ctx context.Context, req sandbox.Request) (sandbox.Result, error) {
	return sandbox.Default().Run(ctx, req)
}

type input struct {
	Operation string `json:"operation"`
	FilePath  string `json:"filePath"`
	Line      int    `json:"line"`
	Character int    `json:"character"`
}

func (t *Tool) Name() string { return "LSP" }

func (t *Tool) Description() string {
	return "Performs LSP operations (goToDefinition, findReferences, hover, documentSymbol) on Go source files using gopls."
}

func (t *Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"operation": {
				"type": "string",
				"enum": ["goToDefinition", "findReferences", "hover", "documentSymbol"],
				"description": "The LSP operation to perform"
			},
			"filePath": {
				"type": "string",
				"description": "Absolute path to the file"
			},
			"line": {
				"type": "number",
				"description": "1-based line number (required for goToDefinition, findReferences, hover)"
			},
			"character": {
				"type": "number",
				"description": "1-based column number (required for goToDefinition, findReferences, hover)"
			}
		},
		"required": ["operation", "filePath"]
	}`)
}

func (t *Tool) IsReadOnly() bool { return true }

func (t *Tool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *Tool) NeedsApproval() bool { return false }

func (t *Tool) TimeoutSeconds() int { return 0 }

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.Operation == "" {
		return agentsdk.ToolResult{Content: "operation is required", IsError: true}, nil
	}
	if in.FilePath == "" {
		return agentsdk.ToolResult{Content: "filePath is required", IsError: true}, nil
	}
	resolvedPath, err := pathutil.ResolveWorkspace(workDir, in.FilePath)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("filePath rejected: %v", err), IsError: true}, nil
	}
	in.FilePath = resolvedPath
	if _, err := exec.LookPath("gopls"); err != nil {
		return agentsdk.ToolResult{Content: "gopls is not installed. Install it with: go install golang.org/x/tools/gopls@latest", IsError: true}, nil
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch in.Operation {
	case "goToDefinition":
		return t.goToDefinition(ctx, in, workDir)
	case "findReferences":
		return t.findReferences(ctx, in, workDir)
	case "hover":
		return t.hover(ctx, in, workDir)
	case "documentSymbol":
		return t.documentSymbol(ctx, in, workDir)
	default:
		return agentsdk.ToolResult{Content: fmt.Sprintf("Unknown operation: %s", in.Operation), IsError: true}, nil
	}
}

// maxLSPFileBytes bounds the size of a source file the LSP tool will buffer
// when translating line/character positions to byte offsets.
const maxLSPFileBytes = 16 << 20 // 16 MiB

func lineCharToByteOffset(filePath string, line, character int) (int, error) {
	data, _, err := pathutil.ReadFileNoFollowLimit(filePath, maxLSPFileBytes)
	if err != nil {
		return 0, fmt.Errorf("reading file: %w", err)
	}

	offset := 0
	currentLine := 1
	for i, b := range data {
		if currentLine == line {
			col := 1
			for j := i; j < len(data) && data[j] != '\n'; j++ {
				if col == character {
					return j, nil
				}
				col++
			}
			if character <= 1 {
				return i, nil
			}
			endOfLine := i
			for endOfLine < len(data) && data[endOfLine] != '\n' {
				endOfLine++
			}
			return endOfLine, nil
		}
		if b == '\n' {
			currentLine++
		}
		offset = i + 1
	}
	return offset, nil
}

func (t *Tool) goToDefinition(ctx context.Context, in input, workDir string) (agentsdk.ToolResult, error) {
	if in.Line == 0 || in.Character == 0 {
		return agentsdk.ToolResult{Content: "line and character are required for goToDefinition", IsError: true}, nil
	}
	byteOffset, err := lineCharToByteOffset(in.FilePath, in.Line, in.Character)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to compute byte offset: %v", err), IsError: true}, nil
	}
	return t.runGopls(ctx, workDir, "definition", fmt.Sprintf("%s:#%d", in.FilePath, byteOffset))
}

func (t *Tool) findReferences(ctx context.Context, in input, workDir string) (agentsdk.ToolResult, error) {
	if in.Line == 0 || in.Character == 0 {
		return agentsdk.ToolResult{Content: "line and character are required for findReferences", IsError: true}, nil
	}
	byteOffset, err := lineCharToByteOffset(in.FilePath, in.Line, in.Character)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to compute byte offset: %v", err), IsError: true}, nil
	}
	return t.runGopls(ctx, workDir, "references", fmt.Sprintf("%s:#%d", in.FilePath, byteOffset))
}

func (t *Tool) hover(ctx context.Context, in input, workDir string) (agentsdk.ToolResult, error) {
	if in.Line == 0 || in.Character == 0 {
		return agentsdk.ToolResult{Content: "line and character are required for hover", IsError: true}, nil
	}
	byteOffset, err := lineCharToByteOffset(in.FilePath, in.Line, in.Character)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to compute byte offset: %v", err), IsError: true}, nil
	}
	return t.runGopls(ctx, workDir, "hover", fmt.Sprintf("%s:#%d", in.FilePath, byteOffset))
}

func (t *Tool) documentSymbol(ctx context.Context, in input, workDir string) (agentsdk.ToolResult, error) {
	return t.runGopls(ctx, workDir, "symbols", in.FilePath)
}

func (t *Tool) runGopls(ctx context.Context, workDir string, args ...string) (agentsdk.ToolResult, error) {
	argv := append([]string{"gopls"}, args...)
	execResult, err := runSandboxCommand(ctx, sandbox.Request{
		Argv:           argv,
		WorkDir:        workDir,
		PermissionMode: policy.PermissionModeReadOnly,
		Timeout:        30 * time.Second,
	})
	outputStr := strings.TrimSpace(execResult.Output)

	if err != nil {
		if outputStr != "" {
			return agentsdk.ToolResult{Content: fmt.Sprintf("gopls error: %s", outputStr), IsError: true}, nil
		}
		return agentsdk.ToolResult{Content: fmt.Sprintf("gopls error: %v", err), IsError: true}, nil
	}
	if execResult.TimedOut {
		return agentsdk.ToolResult{Content: "gopls operation timed out after 30 seconds", IsError: true}, nil
	}
	if execResult.ExitCode != 0 {
		if outputStr != "" {
			return agentsdk.ToolResult{Content: fmt.Sprintf("gopls error: %s", outputStr), IsError: true}, nil
		}
		return agentsdk.ToolResult{Content: fmt.Sprintf("gopls error: exit code %d", execResult.ExitCode), IsError: true}, nil
	}
	if outputStr == "" {
		return agentsdk.ToolResult{Content: "No results found"}, nil
	}
	return agentsdk.ToolResult{Content: outputStr}, nil
}
