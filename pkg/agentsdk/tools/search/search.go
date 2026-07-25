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
	return "List files and directories under a path relative to the working directory. Use glob to find files by name pattern instead of walking directories manually."
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
	return "Read a UTF-8 text file relative to the working directory. Supports line-range slicing via start_line/end_line — prefer ranged reads for large files instead of re-reading the whole file. Use grep to locate content first, then read the relevant range."
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
	data, truncated, err := readFileNoFollowBounded(workDir, path, params.StartLine, params.EndLine)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return agentsdk.ToolResult{Content: notFoundMessage(workDir, params.Path), IsError: true}, nil
		}
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
	return "Find files matching a glob pattern (for example, **/*.go). Returns deterministically sorted, pageable results; use output_format=json for structured completion metadata."
}

func (t *GlobTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string"},"path":{"type":"string","description":"Optional subdirectory to search (default: working directory)"},"limit":{"type":"integer","description":"Max results per page (default 200)"},"cursor":{"type":"string","description":"Continuation cursor returned by a previous identical search"},"include":{"type":"array","items":{"type":"string"},"description":"Additional include globs; all paths must match at least one"},"exclude":{"type":"array","items":{"type":"string"},"description":"Exclude globs"},"respect_gitignore":{"type":"boolean","description":"Honor workspace .gitignore files (default false for compatibility)"},"skip_default_dirs":{"type":"boolean","description":"Skip .git, node_modules, vendor, .venv, and target (default true)"},"output_format":{"type":"string","enum":["text","json"],"description":"text preserves legacy output; json returns matches/truncated/next_cursor"}},"required":["pattern"]}`)
}

func (t *GlobTool) IsReadOnly() bool { return true }

func (t *GlobTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *GlobTool) NeedsApproval() bool { return false }

func (t *GlobTool) TimeoutSeconds() int { return 0 }

func (t *GlobTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Pattern          string   `json:"pattern"`
		Path             string   `json:"path"`
		Limit            int      `json:"limit"`
		Cursor           string   `json:"cursor"`
		Include          []string `json:"include"`
		Exclude          []string `json:"exclude"`
		RespectGitignore bool     `json:"respect_gitignore"`
		SkipDefaultDirs  *bool    `json:"skip_default_dirs"`
		OutputFormat     string   `json:"output_format"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return agentsdk.ToolResult{}, err
	}
	if params.Pattern == "" {
		return agentsdk.ToolResult{Content: "pattern is required", IsError: true}, nil
	}
	if len(params.Pattern) > maxSearchPatternBytes {
		return agentsdk.ToolResult{Content: fmt.Sprintf("pattern must not exceed %d bytes", maxSearchPatternBytes), IsError: true}, nil
	}
	if _, err := filepath.Match(params.Pattern, ""); err != nil {
		return agentsdk.ToolResult{Content: "invalid glob pattern: " + err.Error(), IsError: true}, nil
	}
	if params.OutputFormat != "" && params.OutputFormat != "text" && params.OutputFormat != "json" {
		return agentsdk.ToolResult{Content: "output_format must be text or json", IsError: true}, nil
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultFindLimit
	}
	if limit > maxSearchPageSize {
		return agentsdk.ToolResult{Content: fmt.Sprintf("limit must not exceed %d", maxSearchPageSize), IsError: true}, nil
	}
	root, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	base, err := workspaceRoot(workDir)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	skipDefaultDirs := params.SkipDefaultDirs == nil || *params.SkipDefaultDirs
	filters, err := newPathFilters(base, params.Include, params.Exclude, params.RespectGitignore, skipDefaultDirs)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	query := queryFingerprint("glob", struct {
		Workspace, Pattern, Path          string
		Include, Exclude                  []string
		RespectGitignore, SkipDefaultDirs bool
	}{base, params.Pattern, params.Path, params.Include, params.Exclude, params.RespectGitignore, skipDefaultDirs})
	offset, err := decodeCursor(params.Cursor, query)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	page, truncated, err := collectGlob(root, params.Pattern, filters, offset, limit)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	next := ""
	if truncated {
		next = encodeCursor(query, offset+len(page))
	}
	if params.OutputFormat == "json" {
		content, marshalErr := structuredPage(page, truncated, next, nil, false)
		return agentsdk.ToolResult{Content: content}, marshalErr
	}
	content := strings.Join(page, "\n")
	if content == "" {
		content = "(no matches)"
	}
	return agentsdk.ToolResult{Content: appendTextMetadata(content, len(page), truncated, next)}, nil
}

// GrepTool searches file contents with a regular expression.
type GrepTool struct{}

func (t *GrepTool) Name() string { return "grep" }

func (t *GrepTool) Description() string {
	return "Search file contents with a Go regular expression. Supports pageable structured results, context lines, files/count modes, include/exclude globs, and configurable ignore behavior."
}

func (t *GrepTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"pattern":{"type":"string","description":"Go-syntax regular expression"},"path":{"type":"string","description":"Subdirectory to search (default: working directory)"},"glob":{"type":"string","description":"Legacy single filename include glob"},"include":{"type":"array","items":{"type":"string"},"description":"Include globs"},"exclude":{"type":"array","items":{"type":"string"},"description":"Exclude globs"},"ignore_case":{"type":"boolean"},"limit":{"type":"integer","description":"Max results per page (default 200)"},"cursor":{"type":"string","description":"Continuation cursor returned by a previous identical search"},"before_context":{"type":"integer","minimum":0},"after_context":{"type":"integer","minimum":0},"mode":{"type":"string","enum":["matches","files","count"],"description":"Result mode (default matches)"},"respect_gitignore":{"type":"boolean","description":"Honor workspace .gitignore files (default false for compatibility)"},"skip_default_dirs":{"type":"boolean","description":"Skip .git, node_modules, vendor, .venv, and target (default true)"},"output_format":{"type":"string","enum":["text","json"],"description":"text preserves legacy output; json returns matches/truncated/next_cursor"}},"required":["pattern"]}`)
}

func (t *GrepTool) IsReadOnly() bool { return true }

func (t *GrepTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *GrepTool) NeedsApproval() bool { return false }

func (t *GrepTool) TimeoutSeconds() int { return 0 }

func (t *GrepTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var params struct {
		Pattern          string   `json:"pattern"`
		Path             string   `json:"path"`
		Glob             string   `json:"glob"`
		Include          []string `json:"include"`
		Exclude          []string `json:"exclude"`
		IgnoreCase       bool     `json:"ignore_case"`
		Limit            int      `json:"limit"`
		Cursor           string   `json:"cursor"`
		BeforeContext    int      `json:"before_context"`
		AfterContext     int      `json:"after_context"`
		Mode             string   `json:"mode"`
		RespectGitignore bool     `json:"respect_gitignore"`
		SkipDefaultDirs  *bool    `json:"skip_default_dirs"`
		OutputFormat     string   `json:"output_format"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return agentsdk.ToolResult{}, err
	}
	if params.Pattern == "" {
		return agentsdk.ToolResult{Content: "pattern is required", IsError: true}, nil
	}
	if len(params.Pattern) > maxSearchPatternBytes {
		return agentsdk.ToolResult{Content: fmt.Sprintf("pattern must not exceed %d bytes", maxSearchPatternBytes), IsError: true}, nil
	}
	if params.Glob != "" {
		if _, err := filepath.Match(params.Glob, ""); err != nil {
			return agentsdk.ToolResult{Content: "invalid glob filter: " + err.Error(), IsError: true}, nil
		}
		params.Include = append(params.Include, params.Glob)
	}
	if params.BeforeContext < 0 || params.AfterContext < 0 {
		return agentsdk.ToolResult{Content: "context values must be non-negative", IsError: true}, nil
	}
	if params.BeforeContext > maxSearchContext || params.AfterContext > maxSearchContext {
		return agentsdk.ToolResult{Content: fmt.Sprintf("context values must not exceed %d", maxSearchContext), IsError: true}, nil
	}
	if params.Mode == "" {
		params.Mode = "matches"
	}
	if params.Mode != "matches" && params.Mode != "files" && params.Mode != "count" {
		return agentsdk.ToolResult{Content: "mode must be matches, files, or count", IsError: true}, nil
	}
	if params.OutputFormat != "" && params.OutputFormat != "text" && params.OutputFormat != "json" {
		return agentsdk.ToolResult{Content: "output_format must be text or json", IsError: true}, nil
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
	if limit > maxSearchPageSize {
		return agentsdk.ToolResult{Content: fmt.Sprintf("limit must not exceed %d", maxSearchPageSize), IsError: true}, nil
	}
	root, err := workspacePath(workDir, params.Path)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	base, err := workspaceRoot(workDir)
	if err != nil {
		return agentsdk.ToolResult{}, err
	}
	skipDefaultDirs := params.SkipDefaultDirs == nil || *params.SkipDefaultDirs
	filters, err := newPathFilters(base, params.Include, params.Exclude, params.RespectGitignore, skipDefaultDirs)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	query := queryFingerprint("grep", struct {
		Workspace, Pattern, Path, Mode    string
		Include, Exclude                  []string
		IgnoreCase                        bool
		Before, After                     int
		RespectGitignore, SkipDefaultDirs bool
	}{base, params.Pattern, params.Path, params.Mode, params.Include, params.Exclude, params.IgnoreCase, params.BeforeContext, params.AfterContext, params.RespectGitignore, skipDefaultDirs})
	offset, err := decodeCursor(params.Cursor, query)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	collection, err := collectGrep(workDir, root, base, re, filters, params.BeforeContext, params.AfterContext, params.Mode, offset, limit)
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}, nil
	}
	var page any
	var text string
	var count int
	switch params.Mode {
	case "files":
		page, text, count = collection.Files, strings.Join(collection.Files, "\n"), len(collection.Files)
	case "count":
		page, text, count = collection.Counts, formatCountsText(collection.Counts), len(collection.Counts)
	default:
		page, text, count = collection.Matches, formatGrepText(collection.Matches, params.BeforeContext, params.AfterContext), len(collection.Matches)
	}
	next := ""
	if collection.Truncated {
		next = encodeCursor(query, offset+count)
	}
	if params.OutputFormat == "json" {
		content, marshalErr := structuredPage(page, collection.Truncated, next, collection.Omitted, collection.OmittedTruncated)
		return agentsdk.ToolResult{Content: content}, marshalErr
	}
	for _, omitted := range collection.Omitted {
		if text != "" {
			text += "\n"
		}
		text += "[skipped " + omitted + "]"
	}
	if collection.OmittedTruncated {
		if text != "" {
			text += "\n"
		}
		text += "[additional skipped files omitted from output]"
	}
	if text == "" {
		text = "(no matches)"
	}
	return agentsdk.ToolResult{Content: appendTextMetadata(text, count, collection.Truncated, next)}, nil
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
		rel, ok := relativizeToWorkdir(workDir, clean)
		if !ok {
			return "", fmt.Errorf("path must be relative to workdir: %s", inputPath)
		}
		clean = rel
	}
	return pathutil.ResolveWorkspace(workDir, clean)
}

func relativizeToWorkdir(workDir, absPath string) (string, bool) {
	baseAbs, err := filepath.Abs(workDir)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(baseAbs), filepath.Clean(absPath))
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func workspaceRoot(workDir string) (string, error) {
	return pathutil.ResolveWorkspace(workDir, ".")
}

func notFoundMessage(workDir, requested string) string {
	suggestions := notFoundSuggestions(workDir, requested)
	if suggestions == "" {
		return fmt.Sprintf("%s: no such file — use glob to locate the file", requested)
	}
	return fmt.Sprintf("%s: no such file — did you mean one of: %s", requested, suggestions)
}

func notFoundSuggestions(workDir, requested string) string {
	base := filepath.Base(requested)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return ""
	}
	lower := strings.ToLower(base)
	var exact, partial []string
	visited := 0
	stopWalk := errors.New("stop-walk")
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		visited++
		if visited > 20000 {
			return stopWalk
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		if d.Name() == base {
			exact = append(exact, rel)
			if len(exact) >= 5 {
				return stopWalk
			}
		} else if len(partial) < 5 && strings.Contains(strings.ToLower(d.Name()), lower) {
			partial = append(partial, rel)
		}
		return nil
	})
	candidates := exact
	if len(candidates) == 0 {
		candidates = partial
	}
	return strings.Join(candidates, ", ")
}

func readFileNoFollowBounded(workDir, path string, startLine, endLine int) ([]byte, bool, error) {
	f, err := pathutil.OpenInWorkspace(workDir, path, os.O_RDONLY, 0)
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
	if err := pathutil.RequireSingleLink(info); err != nil {
		return nil, false, fmt.Errorf("refusing workspace read of %s: %w", path, err)
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

func doubleStarMatch(pattern, name string) bool {
	pattern = filepath.ToSlash(pattern)
	name = filepath.ToSlash(name)
	patternParts := strings.Split(pattern, "/")
	nameParts := strings.Split(name, "/")
	type state struct{ pattern, name int }
	memo := make(map[state]bool)
	visited := make(map[state]bool)
	var match func(int, int) bool
	match = func(patternIndex, nameIndex int) bool {
		current := state{patternIndex, nameIndex}
		if visited[current] {
			return memo[current]
		}
		visited[current] = true
		matched := false
		switch {
		case patternIndex == len(patternParts):
			matched = nameIndex == len(nameParts)
		case patternParts[patternIndex] == "**":
			matched = match(patternIndex+1, nameIndex) ||
				(nameIndex < len(nameParts) && match(patternIndex, nameIndex+1))
		case nameIndex < len(nameParts):
			componentMatch, err := filepath.Match(patternParts[patternIndex], nameParts[nameIndex])
			matched = err == nil && componentMatch && match(patternIndex+1, nameIndex+1)
		}
		memo[current] = matched
		return matched
	}
	return match(0, 0)
}
