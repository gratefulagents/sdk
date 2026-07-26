package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

const maxLSPFileBytes = 16 << 20

// Tool provides generic, read-only Language Server Protocol operations.
// Its zero value starts gopls for Go files, preserving the former Tool{}
// behavior. Hosts should set Config or use NewTool for other servers.
type Tool struct {
	Config Config

	mu       sync.Mutex
	managers map[string]*Manager
	closed   bool
}

// NewTool creates a read-only LSP tool for config's stdio language server.
func NewTool(config Config) *Tool { return &Tool{Config: config} }

func (t *Tool) Name() string { return "LSP" }

func (t *Tool) Description() string {
	return "Performs read-only LSP operations (definition, references, hover, document/workspace symbols, implementation, type definition, diagnostics) using a configured language server."
}

func (t *Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"operation":{"type":"string","enum":["goToDefinition","definition","findReferences","references","hover","documentSymbol","workspaceSymbol","implementation","typeDefinition","diagnostics"],"description":"The read-only LSP operation to perform"},
			"filePath":{"type":"string","description":"Workspace-relative or absolute path to the source file"},
			"languageId":{"type":"string","description":"Language identifier used to select the configured server"},
			"line":{"type":"number","description":"1-based source line for position-based operations"},
			"character":{"type":"number","description":"1-based UTF-16 code-unit position for position-based operations"},
			"query":{"type":"string","description":"Search query for workspaceSymbol"}
		},
		"required":["operation"]
	}`)
}

func (t *Tool) IsReadOnly() bool                      { return true }
func (t *Tool) IsEnabled(_ *agentsdk.RunContext) bool { return true }
func (t *Tool) NeedsApproval() bool                   { return false }
func (t *Tool) TimeoutSeconds() int                   { return 0 }

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var input struct {
		Operation  string `json:"operation"`
		FilePath   string `json:"filePath"`
		LanguageID string `json:"languageId"`
		Line       int    `json:"line"`
		Character  int    `json:"character"`
		Query      string `json:"query"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	request := Request{
		Operation:  input.Operation,
		FilePath:   input.FilePath,
		LanguageID: input.LanguageID,
		Line:       input.Line,
		Character:  input.Character,
		Query:      input.Query,
	}
	manager, err := t.getManager(ctx, workDir, request)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}
	result, err := manager.Execute(ctx, workDir, request)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}
	content, err := marshalResult(result, manager.config.MaxOutputBytes)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("LSP error: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: content}, nil
}

// Close terminates all configured/discovered language-server sessions.
func (t *Tool) Close() error {
	t.mu.Lock()
	managers := make([]*Manager, 0, len(t.managers))
	for _, manager := range t.managers {
		managers = append(managers, manager)
	}
	t.managers = nil
	t.closed = true
	t.mu.Unlock()
	var errs []error
	for _, manager := range managers {
		if err := manager.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (t *Tool) getManager(ctx context.Context, workDir string, request Request) (*Manager, error) {
	t.mu.Lock()
	closed := t.closed
	t.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("LSP tool is closed")
	}
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return nil, err
	}
	root := t.Config
	candidates := make([]Config, 0, 1+len(root.AdditionalServers))
	useGoCompatibilityDefault := root.Command == "" && root.LanguageID == "" && len(root.FilePatterns) == 0 && len(root.AdditionalServers) == 0 && root.Discoverer == nil
	if !useGoCompatibilityDefault && (root.LanguageID != "" || len(root.FilePatterns) != 0) && strings.TrimSpace(root.Command) == "" {
		return nil, fmt.Errorf("configured language server command is required")
	}
	if useGoCompatibilityDefault || root.Command != "" || root.LanguageID != "" || len(root.FilePatterns) != 0 {
		base := root
		base.AdditionalServers = nil
		base.Discoverer = nil
		candidates = append(candidates, base)
	}
	for _, config := range root.AdditionalServers {
		if strings.TrimSpace(config.Command) == "" {
			return nil, fmt.Errorf("additional language server command is required")
		}
		config.AdditionalServers = nil
		config.Discoverer = nil
		config.Executor = root.Executor
		config.AllowUnsafeUnconfined = false
		candidates = append(candidates, config)
	}
	if root.Discoverer != nil {
		discovered, err := root.Discoverer.DiscoverLSPServers(ctx, workspace, request)
		if err != nil {
			return nil, fmt.Errorf("discovering LSP servers: %w", err)
		}
		for _, config := range discovered {
			if strings.TrimSpace(config.Command) == "" {
				return nil, fmt.Errorf("discovered language server command is required")
			}
			config.AdditionalServers = nil
			config.Discoverer = nil
			config.Executor = root.Executor
			config.AllowUnsafeUnconfined = false
			candidates = append(candidates, config)
		}
	}
	type match struct {
		key string
		cfg Config
	}
	matches := make([]match, 0, len(candidates))
	for index, config := range candidates {
		config = config.normalized()
		if err := matchesConfig(config, workspace, request); err != nil {
			continue
		}
		key := config.ID
		if key == "" {
			key = fmt.Sprintf("%s|%q|%s|%q|%d", config.Command, config.Args, config.LanguageID, config.FilePatterns, index)
		}
		matches = append(matches, match{key: key, cfg: config})
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("no configured language server matches the request")
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, candidate := range matches {
			ids = append(ids, candidate.key)
		}
		return nil, fmt.Errorf("language-server selection is ambiguous: %s", strings.Join(ids, ", "))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, fmt.Errorf("LSP tool is closed")
	}
	if t.managers == nil {
		t.managers = make(map[string]*Manager)
	}
	manager := t.managers[matches[0].key]
	if manager == nil {
		manager = NewManager(matches[0].cfg)
		t.managers[matches[0].key] = manager
	}
	return manager, nil
}

func executeRequest(ctx context.Context, s *session, config Config, workspace string, request Request) (Result, error) {
	s.operationMu.Lock()
	defer s.operationMu.Unlock()

	operation, err := normalizeOperation(request.Operation)
	if err != nil {
		return Result{}, err
	}
	if err := matchesConfig(config, workspace, request); err != nil {
		return Result{}, err
	}

	var document *document
	if requiresDocument(operation) {
		if request.FilePath == "" {
			return Result{}, fmt.Errorf("filePath is required for %s", request.Operation)
		}
		document, err = openDocument(workspace, request.FilePath, languageID(config, request))
		if err != nil {
			return Result{}, err
		}
		opened, exists := s.opened[document.path]
		if !exists {
			s.clearDiagnostics(document.path)
			if err := s.notify(ctx, "textDocument/didOpen", map[string]any{
				"textDocument": map[string]any{
					"uri":        fileURI(document.path),
					"languageId": document.languageID,
					"version":    1,
					"text":       document.text,
				},
			}); err != nil {
				return Result{}, fmt.Errorf("opening LSP document: %w", err)
			}
			s.opened[document.path] = openedDocument{text: document.text, version: 1}
		} else if opened.text != document.text {
			opened.version++
			s.clearDiagnostics(document.path)
			if err := s.notify(ctx, "textDocument/didChange", map[string]any{
				"textDocument":   map[string]any{"uri": fileURI(document.path), "version": opened.version},
				"contentChanges": []map[string]string{{"text": document.text}},
			}); err != nil {
				return Result{}, fmt.Errorf("updating LSP document: %w", err)
			}
			opened.text = document.text
			s.opened[document.path] = opened
		}
		defer s.closeDocument(ctx, document.path)
	} else {
		s.closeAllDocuments(ctx)
	}

	if operation == "diagnostics" && !s.pullDiagnostics {
		diagnostics, err := s.waitForPublishedDiagnostics(ctx, document.path, s.opened[document.path].version)
		if err != nil {
			return Result{}, err
		}
		return Result{Operation: operation, Diagnostics: diagnostics}, nil
	}

	var method string
	var params any
	switch operation {
	case "definition", "references", "hover", "implementation", "typeDefinition":
		position, err := document.position(request.Line, request.Character)
		if err != nil {
			return Result{}, err
		}
		method = map[string]string{
			"definition":     "textDocument/definition",
			"references":     "textDocument/references",
			"hover":          "textDocument/hover",
			"implementation": "textDocument/implementation",
			"typeDefinition": "textDocument/typeDefinition",
		}[operation]
		params = map[string]any{
			"textDocument": map[string]string{"uri": fileURI(document.path)},
			"position":     position,
		}
		if operation == "references" {
			params.(map[string]any)["context"] = map[string]bool{"includeDeclaration": true}
		}
	case "documentSymbol":
		method = "textDocument/documentSymbol"
		params = map[string]any{"textDocument": map[string]string{"uri": fileURI(document.path)}}
	case "workspaceSymbol":
		method = "workspace/symbol"
		params = map[string]string{"query": request.Query}
	case "diagnostics":
		method = "textDocument/diagnostic"
		params = map[string]any{"textDocument": map[string]string{"uri": fileURI(document.path)}}
	}

	raw, err := s.call(ctx, 2, method, params)
	if err != nil {
		if operation == "diagnostics" && document != nil {
			if published, ok := s.publishedDiagnostics(document.path, s.opened[document.path].version); ok {
				return Result{Operation: operation, Diagnostics: published}, nil
			}
		}
		return Result{}, err
	}
	result, err := parseResult(operation, raw, workspace)
	if err != nil {
		return Result{}, err
	}
	if operation == "diagnostics" && document != nil {
		for index := range result.Diagnostics {
			result.Diagnostics[index].FilePath = document.path
		}
	}
	return result, nil
}

func (s *session) clearDiagnostics(path string) {
	s.diagnosticsMu.Lock()
	delete(s.diagnostics, path)
	delete(s.diagnosticVersions, path)
	s.diagnosticsMu.Unlock()
}

func (s *session) publishedDiagnostics(path string, minimumVersion int) ([]Diagnostic, bool) {
	s.diagnosticsMu.Lock()
	defer s.diagnosticsMu.Unlock()
	diagnostics, ok := s.diagnostics[path]
	if !ok {
		return nil, false
	}
	if version, versioned := s.diagnosticVersions[path]; versioned && version < minimumVersion {
		return nil, false
	}
	return append([]Diagnostic(nil), diagnostics...), true
}

func (s *session) waitForPublishedDiagnostics(ctx context.Context, path string, minimumVersion int) ([]Diagnostic, error) {
	for {
		if diagnostics, ok := s.publishedDiagnostics(path, minimumVersion); ok {
			return diagnostics, nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for published diagnostics: %w", ctx.Err())
		case <-s.diagnosticWake:
		case <-s.waitDone:
			return nil, fmt.Errorf("LSP server exited while waiting for diagnostics: %w", s.processErr())
		}
	}
}

func (s *session) closeDocument(ctx context.Context, path string) {
	if _, ok := s.opened[path]; !ok {
		return
	}
	_ = s.notify(ctx, "textDocument/didClose", map[string]any{
		"textDocument": map[string]string{"uri": fileURI(path)},
	})
	delete(s.opened, path)
	s.clearDiagnostics(path)
}

func (s *session) closeAllDocuments(ctx context.Context) {
	paths := make([]string, 0, len(s.opened))
	for path := range s.opened {
		paths = append(paths, path)
	}
	for _, path := range paths {
		s.closeDocument(ctx, path)
	}
}

func normalizeOperation(operation string) (string, error) {
	switch operation {
	case "goToDefinition", "definition":
		return "definition", nil
	case "findReferences", "references":
		return "references", nil
	case "hover", "documentSymbol", "workspaceSymbol", "implementation", "typeDefinition", "diagnostics":
		return operation, nil
	default:
		return "", fmt.Errorf("unknown or non-read-only LSP operation: %s", operation)
	}
}

func requiresDocument(operation string) bool { return operation != "workspaceSymbol" }

func matchesConfig(config Config, workspace string, request Request) error {
	if config.LanguageID != "" && request.LanguageID != "" && config.LanguageID != request.LanguageID {
		return fmt.Errorf("languageId %q does not match configured server languageId %q", request.LanguageID, config.LanguageID)
	}
	if len(config.FilePatterns) == 0 || request.FilePath == "" {
		return nil
	}
	path, err := pathutil.ResolveWorkspace(workspace, request.FilePath)
	if err != nil {
		return fmt.Errorf("filePath rejected: %w", err)
	}
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return err
	}
	for _, pattern := range config.FilePatterns {
		if matchesFilePattern(pattern, filepath.ToSlash(rel)) {
			return nil
		}
	}
	return fmt.Errorf("filePath %q does not match configured language-server file patterns", request.FilePath)
}

func matchesFilePattern(pattern, relative string) bool {
	pattern = filepath.ToSlash(pattern)
	if ok, _ := filepath.Match(pattern, relative); ok {
		return true
	}
	if ok, _ := filepath.Match(pattern, filepath.Base(relative)); ok {
		return true
	}
	if rest, ok := strings.CutPrefix(pattern, "**/"); ok {
		return matchesFilePattern(rest, relative)
	}
	return false
}

func languageID(config Config, request Request) string {
	if config.LanguageID != "" {
		return config.LanguageID
	}
	return request.LanguageID
}

type document struct {
	path       string
	languageID string
	text       string
}

func openDocument(workspace, filePath, languageID string) (*document, error) {
	path, err := pathutil.ResolveWorkspace(workspace, filePath)
	if err != nil {
		return nil, fmt.Errorf("filePath rejected: %w", err)
	}
	file, err := pathutil.OpenInWorkspace(workspace, filePath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("opening filePath: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stating filePath: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("filePath is not a regular file")
	}
	if err := pathutil.RequireSingleLink(info); err != nil {
		return nil, fmt.Errorf("filePath rejected: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLSPFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading filePath: %w", err)
	}
	if len(data) > maxLSPFileBytes {
		return nil, fmt.Errorf("filePath exceeds %d-byte LSP source limit", maxLSPFileBytes)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("filePath is not valid UTF-8")
	}
	return &document{path: path, languageID: languageID, text: string(data)}, nil
}

func (d *document) position(line, character int) (map[string]int, error) {
	if line < 1 || character < 1 {
		return nil, errorsPosition("line and character must be 1-based positive integers")
	}
	lines := strings.Split(d.text, "\n")
	if line > len(lines) {
		return nil, errorsPosition("line is outside the file")
	}
	selected := strings.TrimSuffix(lines[line-1], "\r")
	utf16Length := len(utf16.Encode([]rune(selected)))
	if character > utf16Length+1 {
		return nil, errorsPosition("character is outside the line")
	}
	return map[string]int{"line": line - 1, "character": character - 1}, nil
}

type errorsPosition string

func (e errorsPosition) Error() string { return string(e) }

func canonicalWorkspace(workDir string) (string, error) {
	workspace, err := pathutil.ResolveWorkspace(workDir, ".")
	if err != nil {
		return "", fmt.Errorf("resolving workspace: %w", err)
	}
	return workspace, nil
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

func marshalResult(result Result, maxBytes int) (string, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	if len(data) > maxBytes {
		return "", fmt.Errorf("LSP result exceeds configured output limit of %d bytes", maxBytes)
	}
	return string(data), nil
}
