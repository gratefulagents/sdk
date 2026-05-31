package search

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const (
	defaultListLimit = 100
	defaultFindLimit = 200
	maxReadBytes     = 100_000
	maxLineBytes     = 400
)

// ListFilesTool lists files and directories under a workspace-relative path.
type ListFilesTool struct{}

func (t *ListFilesTool) Name() string { return "list_files" }

func (t *ListFilesTool) Description() string {
	return "List files and directories under a path relative to the working directory."
}

func (t *ListFilesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Relative path (default: working directory)"},"limit":{"type":"integer","description":"Max entries (default 100)"}}}`)
}

func (t *ListFilesTool) IsReadOnly() bool { return true }

func (t *ListFilesTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ListFilesTool) NeedsApproval() bool { return false }

func (t *ListFilesTool) TimeoutSeconds() int { return 0 }

func (t *ListFilesTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return agentsdk.ToolResult{}, err
		}
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	dir, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > limit {
		names = names[:limit]
	}
	return agentsdk.ToolResult{Content: strings.Join(names, "\n")}, nil
}

// ReadFileTool reads a UTF-8 text file under the workspace.
type ReadFileTool struct{}

func (t *ReadFileTool) Name() string { return "read_file" }

func (t *ReadFileTool) Description() string {
	return "Read a UTF-8 text file relative to the working directory. Supports line-range slicing."
}

func (t *ReadFileTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"start_line":{"type":"integer","description":"1-indexed start line"},"end_line":{"type":"integer","description":"1-indexed end line (inclusive); 0 = end of file"}},"required":["path"]}`)
}

func (t *ReadFileTool) IsReadOnly() bool { return true }

func (t *ReadFileTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *ReadFileTool) NeedsApproval() bool { return false }

func (t *ReadFileTool) TimeoutSeconds() int { return 0 }

func (t *ReadFileTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		EndLine   int    `json:"end_line"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return agentsdk.ToolResult{}, err
	}
	path, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	data, truncated, err := readFileNoFollowBounded(path, params.StartLine, params.EndLine)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	out := string(data)
	if truncated {
		out += "\n[output truncated]"
	}
	return agentsdk.ToolResult{Content: out}, nil
}

// GlobTool finds files whose relative path matches a glob pattern.
type GlobTool struct{}

func (t *GlobTool) Name() string { return "glob" }

func (t *GlobTool) Description() string {
	return "Find files matching a glob pattern (for example, **/*.go). Returns sorted relative paths."
}

func (t *GlobTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"Optional subdirectory to search (default: working directory)"},"limit":{"type":"integer","description":"Max results (default 200)"}},"required":["pattern"]}`)
}

func (t *GlobTool) IsReadOnly() bool { return true }

func (t *GlobTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *GlobTool) NeedsApproval() bool { return false }

func (t *GlobTool) TimeoutSeconds() int { return 0 }

func (t *GlobTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return agentsdk.ToolResult{}, err
	}
	if params.Pattern == "" {
		return agentsdk.ToolResult{Content: "pattern is required", IsError: true}, nil
	}
	if _, err := filepath.Match(params.Pattern, ""); err != nil {
		return agentsdk.ToolResult{Content: "invalid glob pattern: " + err.Error(), IsError: true}, nil
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultFindLimit
	}
	root, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	base, err := workspaceRoot(workDir)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	matches, err := globWalk(root, params.Pattern, limit)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	rel := make([]string, 0, len(matches))
	for _, match := range matches {
		r, relErr := filepath.Rel(base, match)
		if relErr != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("computing relative path for %s: %v", match, relErr), IsError: true}, nil
		}
		rel = append(rel, r)
	}
	sort.Strings(rel)
	if len(rel) == 0 {
		return agentsdk.ToolResult{Content: "(no matches)"}, nil
	}
	return agentsdk.ToolResult{Content: strings.Join(rel, "\n")}, nil
}

// GrepTool searches file contents with a regular expression.
type GrepTool struct{}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "Search file contents with a regular expression. Returns matching lines with file:line prefixes."
}

func (t *GrepTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go-syntax regular expression"},"path":{"type":"string","description":"Subdirectory to search (default: working directory)"},"glob":{"type":"string","description":"Optional filename glob filter (for example, *.go)"},"ignore_case":{"type":"boolean"},"limit":{"type":"integer","description":"Max matches returned (default 200)"}},"required":["pattern"]}`)
}

func (t *GrepTool) IsReadOnly() bool { return true }

func (t *GrepTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *GrepTool) NeedsApproval() bool { return false }

func (t *GrepTool) TimeoutSeconds() int { return 0 }

func (t *GrepTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
		Limit      int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return agentsdk.ToolResult{}, err
	}
	if params.Pattern == "" {
		return agentsdk.ToolResult{Content: "pattern is required", IsError: true}, nil
	}
	if params.Glob != "" {
		if _, err := filepath.Match(params.Glob, ""); err != nil {
			return agentsdk.ToolResult{Content: "invalid glob filter: " + err.Error(), IsError: true}, nil
		}
	}
	pattern := params.Pattern
	if params.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return agentsdk.ToolResult{Content: "invalid regex: " + err.Error(), IsError: true}, nil
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultFindLimit
	}
	root, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	base, err := workspaceRoot(workDir)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	out, err := grepWalk(root, base, re, params.Glob, limit)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	if out == "" {
		return agentsdk.ToolResult{Content: "(no matches)"}, nil
	}
	return agentsdk.ToolResult{Content: out}, nil
}

// DefaultTools returns the SDK's generic read-only workspace discovery tools.
func DefaultTools() []agentsdk.Tool {
	return []agentsdk.Tool{
		&ListFilesTool{},
		&ReadFileTool{},
		&GlobTool{},
		&GrepTool{},
	}
}

func workspacePath(workDir, inputPath string) (string, error) {
	clean := strings.TrimSpace(inputPath)
	if clean == "" {
		clean = "."
	}
	if filepath.IsAbs(clean) {
		return "", fmt.Errorf("path must be relative to workdir: %s", inputPath)
	}
	return pathutil.ResolveWorkspace(workDir, clean)
}

func workspaceRoot(workDir string) (string, error) {
	return pathutil.ResolveWorkspace(workDir, ".")
}

func readFileNoFollowBounded(path string, startLine, endLine int) ([]byte, bool, error) {
	f, err := pathutil.OpenFileNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", path)
	}

	if startLine <= 0 && endLine <= 0 {
		data, err := io.ReadAll(io.LimitReader(f, maxReadBytes+1))
		if err != nil {
			return nil, false, err
		}
		if len(data) > maxReadBytes {
			return data[:maxReadBytes], true, nil
		}
		return data, false, nil
	}

	start := startLine
	if start < 1 {
		start = 1
	}
	if endLine > 0 && endLine < start {
		return []byte{}, false, nil
	}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxReadBytes+1)
	lineNum := 0
	var b strings.Builder
	truncated := false
	for scanner.Scan() {
		lineNum++
		if lineNum < start {
			continue
		}
		if endLine > 0 && lineNum > endLine {
			break
		}
		if b.Len() > 0 {
			if b.Len()+1 > maxReadBytes {
				truncated = true
				break
			}
			b.WriteByte('\n')
		}
		line := scanner.Text()
		remaining := maxReadBytes - b.Len()
		if len(line) > remaining {
			b.WriteString(line[:remaining])
			truncated = true
			break
		}
		b.WriteString(line)
	}
	if err := scanner.Err(); err != nil {
		return nil, false, fmt.Errorf("scan %s: %w", path, err)
	}
	return []byte(b.String()), truncated, nil
}

func shouldSkipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".venv", "target":
		return true
	default:
		return false
	}
}

func globWalk(root, pattern string, limit int) ([]string, error) {
	var matches []string
	doublestar := strings.Contains(pattern, "**")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}
		var ok bool
		if doublestar {
			ok = doubleStarMatch(pattern, rel)
		} else {
			matched, matchErr := filepath.Match(pattern, filepath.Base(rel))
			if matchErr != nil {
				return matchErr
			}
			if matched {
				ok = true
			} else {
				matched, matchErr = filepath.Match(pattern, rel)
				if matchErr != nil {
					return matchErr
				}
				ok = matched
			}
		}
		if ok {
			matches = append(matches, path)
			if len(matches) >= limit {
				return errors.New("limit-reached")
			}
		}
		return nil
	})
	if err != nil && err.Error() != "limit-reached" {
		return nil, err
	}
	return matches, nil
}

func doubleStarMatch(pattern, name string) bool {
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		if ok, _ := filepath.Match(rest, name); ok {
			return true
		}
	}
	parts := strings.Split(pattern, "**")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	rePat := "^" + strings.Join(parts, ".*") + "$"
	rePat = strings.ReplaceAll(rePat, `\*`, "[^/]*")
	rePat = strings.ReplaceAll(rePat, `\?`, ".")
	re, err := regexp.Compile(rePat)
	if err != nil {
		return false
	}
	return re.MatchString(name)
}

func grepWalk(root, base string, re *regexp.Regexp, glob string, limit int) (string, error) {
	var b strings.Builder
	count := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if glob != "" {
			ok, err := filepath.Match(glob, d.Name())
			if err != nil {
				return err
			}
			if !ok {
				return nil
			}
		}
		f, err := pathutil.OpenFileNoFollow(path, os.O_RDONLY, 0)
		if err != nil {
			return nil
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil
		}

		rel, err := filepath.Rel(base, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		lineNum := 0
		for scanner.Scan() {
			lineNum++
			line := scanner.Text()
			if re.MatchString(line) {
				if len(line) > maxLineBytes {
					line = line[:maxLineBytes] + "..."
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", rel, lineNum, line)
				count++
				if count >= limit {
					return errors.New("limit-reached")
				}
			}
		}
		if scanErr := scanner.Err(); scanErr != nil {
			_ = f.Close()
			return fmt.Errorf("scanning %s: %w", path, scanErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return fmt.Errorf("closing %s: %w", path, closeErr)
		}
		return nil
	})
	if err != nil && err.Error() != "limit-reached" {
		return "", err
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
