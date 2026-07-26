package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

const (
	defaultStartupTimeout = 10 * time.Second
	defaultRequestTimeout = 15 * time.Second
	defaultMaxMessageSize = 1 << 20
	defaultMaxOutputSize  = 1 << 20
	defaultMaxStderrSize  = 64 << 10
	defaultMaxMessages    = 16
)

// Config configures one stdio Language Server Protocol server. A host can
// create one Tool or Manager per server and select the appropriate language
// server with LanguageID and FilePatterns.
type Config struct {
	// ID uniquely identifies this server when Config.AdditionalServers or a
	// Discoverer supplies more than one candidate.
	ID      string
	Command string
	Args    []string
	Env     []string

	// Executor confines the language-server process. Registry/runtime wiring
	// always supplies the configured SDK command sandbox in read-only mode.
	// Standalone callers should also provide an enforcing executor.
	Executor sandbox.Executor
	// AllowUnsafeUnconfined permits direct execution without the SDK command
	// sandbox. It is intended only for explicitly trusted standalone test/host
	// processes; registry/runtime wiring never enables it.
	AllowUnsafeUnconfined bool

	LanguageID   string
	FilePatterns []string

	StartupTimeout  time.Duration
	RequestTimeout  time.Duration
	MaxMessageBytes int
	MaxOutputBytes  int
	MaxStderrBytes  int
	MaxMessages     int

	// AdditionalServers and Discoverer let one LSP tool route across language
	// servers. Discovery is host-owned; the SDK never hard-codes installations.
	AdditionalServers []Config
	Discoverer        ServerDiscoverer
}

// ServerDiscoverer returns trusted host-configured server candidates for a
// request. Returned commands are still launched through the read-only sandbox.
type ServerDiscoverer interface {
	DiscoverLSPServers(context.Context, string, Request) ([]Config, error)
}

func (c Config) normalized() Config {
	if strings.TrimSpace(c.Command) == "" {
		c.Command = "gopls"
		if c.LanguageID == "" {
			c.LanguageID = "go"
		}
		if len(c.FilePatterns) == 0 {
			c.FilePatterns = []string{"*.go"}
		}
	}
	if c.StartupTimeout <= 0 {
		c.StartupTimeout = defaultStartupTimeout
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = defaultMaxMessageSize
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = defaultMaxOutputSize
	}
	if c.MaxStderrBytes <= 0 {
		c.MaxStderrBytes = defaultMaxStderrSize
	}
	if c.MaxMessages <= 0 {
		c.MaxMessages = defaultMaxMessages
	}
	return c
}

// Request is a read-only LSP operation. Line is one-based and Character is a
// one-based UTF-16 code-unit position, matching the stable result contract.
type Request struct {
	Operation  string
	FilePath   string
	LanguageID string
	Line       int
	Character  int
	Query      string
}

// Position is a one-based source position whose Character is measured in
// UTF-16 code units, so result positions can be passed back into requests.
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range identifies a one-based source range in a workspace file.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a workspace-confined source location.
type Location struct {
	FilePath string `json:"filePath"`
	Range    Range  `json:"range"`
}

// Hover is the stable representation of textDocument/hover output.
type Hover struct {
	Contents string `json:"contents"`
	Range    *Range `json:"range,omitempty"`
}

// Symbol is a workspace-confined document or workspace symbol.
type Symbol struct {
	Name           string    `json:"name"`
	Kind           int       `json:"kind"`
	Detail         string    `json:"detail,omitempty"`
	Range          *Range    `json:"range,omitempty"`
	Location       *Location `json:"location,omitempty"`
	SelectionRange *Range    `json:"selectionRange,omitempty"`
	Children       []Symbol  `json:"children,omitempty"`
}

// Diagnostic is a workspace-confined diagnostic returned by the language server.
type Diagnostic struct {
	FilePath string `json:"filePath"`
	Range    Range  `json:"range"`
	Severity int    `json:"severity,omitempty"`
	Code     string `json:"code,omitempty"`
	Source   string `json:"source,omitempty"`
	Message  string `json:"message"`
}

// Result is the stable structured output for all read-only LSP operations.
type Result struct {
	Operation   string       `json:"operation"`
	Locations   []Location   `json:"locations,omitempty"`
	Hover       *Hover       `json:"hover,omitempty"`
	Symbols     []Symbol     `json:"symbols,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// Manager owns workspace-scoped stdio LSP sessions. Close terminates every
// child process and releases all pipes.
type Manager struct {
	config Config

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// NewManager creates a generic stdio LSP manager. A zero Config deliberately
// retains the previous gopls behavior for callers migrating from Tool{}.
func NewManager(config Config) *Manager {
	return &Manager{config: config.normalized(), sessions: make(map[string]*session)}
}

// Execute performs one read-only LSP request in a session scoped to workDir.
func (m *Manager) Execute(ctx context.Context, workDir string, request Request) (Result, error) {
	workspace, err := canonicalWorkspace(workDir)
	if err != nil {
		return Result{}, err
	}
	operation, err := normalizeOperation(request.Operation)
	if err != nil {
		return Result{}, err
	}
	if err := matchesConfig(m.config, workspace, request); err != nil {
		return Result{}, err
	}
	if requiresDocument(operation) && request.FilePath == "" {
		return Result{}, fmt.Errorf("filePath is required for %s", request.Operation)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Result{}, errors.New("LSP manager is closed")
	}
	s := m.sessions[workspace]
	if s != nil && s.isClosed() {
		delete(m.sessions, workspace)
		s = nil
	}
	if s == nil {
		s, err = startSession(ctx, m.config, workspace)
		if err == nil {
			m.sessions[workspace] = s
		}
	}
	m.mu.Unlock()
	if err != nil {
		return Result{}, err
	}

	requestCtx, cancel := context.WithTimeout(ctx, m.config.RequestTimeout)
	defer cancel()
	return executeRequest(requestCtx, s, m.config, workspace, request)
}

// Close terminates all language server processes. It is safe to call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	sessions := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		sessions = append(sessions, s)
	}
	m.sessions = nil
	m.mu.Unlock()

	var errs []error
	for _, s := range sessions {
		if err := s.close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type boundedBuffer struct {
	mu     sync.Mutex
	buf    []byte
	limit  int
	capped bool
}

func newBoundedBuffer(limit int) *boundedBuffer { return &boundedBuffer{limit: limit} }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) >= b.limit {
		b.capped = true
		return len(p), nil
	}
	remaining := b.limit - len(b.buf)
	if len(p) > remaining {
		b.buf = append(b.buf, p[:remaining]...)
		b.capped = true
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := string(b.buf)
	if b.capped {
		out += "\n[stderr truncated]"
	}
	return out
}

type inbound struct {
	message rpcMessage
	err     error
}

type openedDocument struct {
	text    string
	version int
}

type session struct {
	config    Config
	workspace string
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    *boundedBuffer
	cancel    context.CancelFunc

	messages chan inbound
	waitDone chan struct{}
	waitMu   sync.Mutex
	waitErr  error

	operationMu        sync.Mutex
	requestMu          sync.Mutex
	writeMu            sync.Mutex
	nextID             int
	opened             map[string]openedDocument
	diagnosticsMu      sync.Mutex
	diagnostics        map[string][]Diagnostic
	diagnosticVersions map[string]int
	diagnosticWake     chan struct{}
	pullDiagnostics    bool
	stateMu            sync.Mutex
	closed             bool
	closeOnce          sync.Once
}

func startSession(ctx context.Context, config Config, workspace string) (*session, error) {
	startupCtx, cancelStartup := context.WithTimeout(ctx, config.StartupTimeout)
	defer cancelStartup()

	processCtx, cancelProcess := context.WithCancel(context.Background())
	var (
		cmd *exec.Cmd
		err error
	)
	if config.Executor != nil {
		env := make(map[string]string, len(config.Env))
		for _, assignment := range config.Env {
			name, value, ok := strings.Cut(assignment, "=")
			if ok && name != "" {
				env[name] = value
			}
		}
		argv := append([]string{config.Command}, config.Args...)
		cmd, err = config.Executor.Build(processCtx, sandbox.Request{
			Argv: argv, WorkDir: workspace,
			PermissionMode: policy.PermissionModeReadOnly,
			Env:            env,
		})
		if err != nil {
			cancelProcess()
			return nil, fmt.Errorf("building confined LSP server command %q: %w", config.Command, err)
		}
	} else {
		if !config.AllowUnsafeUnconfined {
			cancelProcess()
			return nil, fmt.Errorf("LSP server %q requires a read-only sandbox executor", config.Command)
		}
		cmd = exec.CommandContext(processCtx, config.Command, config.Args...)
		cmd.Dir = workspace
		cmd.Env = append(os.Environ(), config.Env...)
	}
	configureProcess(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancelProcess()
		return nil, fmt.Errorf("creating LSP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancelProcess()
		return nil, fmt.Errorf("creating LSP stdout: %w", err)
	}
	stderr := newBoundedBuffer(config.MaxStderrBytes)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cancelProcess()
		return nil, fmt.Errorf("starting LSP server %q: %w", config.Command, err)
	}

	s := &session{
		config:             config,
		workspace:          workspace,
		cmd:                cmd,
		stdin:              stdin,
		stdout:             stdout,
		stderr:             stderr,
		cancel:             cancelProcess,
		messages:           make(chan inbound, config.MaxMessages),
		opened:             make(map[string]openedDocument),
		diagnostics:        make(map[string][]Diagnostic),
		diagnosticVersions: make(map[string]int),
		diagnosticWake:     make(chan struct{}, 1),
		waitDone:           make(chan struct{}),
	}
	go s.readLoop()

	params := map[string]any{
		"processId": nil,
		"rootUri":   fileURI(workspace),
		"workspaceFolders": []map[string]string{{
			"uri":  fileURI(workspace),
			"name": filepath.Base(workspace),
		}},
		"capabilities": map[string]any{
			"general": map[string]any{"positionEncodings": []string{"utf-16"}},
			"textDocument": map[string]any{
				"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true},
			},
		},
	}
	initializeResult, err := s.call(startupCtx, 1, "initialize", params)
	if err != nil {
		stderrText := s.stderr.String()
		_ = s.close()
		if stderrText != "" {
			return nil, fmt.Errorf("initializing LSP server: %w: %s", err, stderrText)
		}
		return nil, fmt.Errorf("initializing LSP server: %w", err)
	}
	var initialized struct {
		Capabilities struct {
			PositionEncoding   string          `json:"positionEncoding"`
			DiagnosticProvider json.RawMessage `json:"diagnosticProvider"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(initializeResult, &initialized); err != nil {
		_ = s.close()
		return nil, fmt.Errorf("decoding LSP initialize result: %w", err)
	}
	if encoding := strings.ToLower(initialized.Capabilities.PositionEncoding); encoding != "" && encoding != "utf-16" {
		_ = s.close()
		return nil, fmt.Errorf("LSP server selected unsupported position encoding %q", encoding)
	}
	diagnosticCapability := strings.TrimSpace(string(initialized.Capabilities.DiagnosticProvider))
	s.pullDiagnostics = diagnosticCapability != "" && diagnosticCapability != "null" && diagnosticCapability != "false"
	if err := s.notify(startupCtx, "initialized", map[string]any{}); err != nil {
		_ = s.close()
		return nil, fmt.Errorf("sending LSP initialized notification: %w", err)
	}
	return s, nil
}

func (s *session) readLoop() {
	defer func() {
		s.stateMu.Lock()
		s.closed = true
		s.stateMu.Unlock()
		s.waitMu.Lock()
		s.waitErr = s.cmd.Wait()
		s.waitMu.Unlock()
		close(s.waitDone)
	}()

	reader := bufio.NewReader(s.stdout)
	for {
		message, err := readRPCMessage(reader, s.config.MaxMessageBytes)
		if err != nil {
			select {
			case s.messages <- inbound{err: err}:
			default:
			}
			s.terminate()
			return
		}
		if message.Method != "" {
			handleCtx, cancel := context.WithTimeout(context.Background(), s.config.RequestTimeout)
			_ = s.handleServerMessage(handleCtx, message)
			cancel()
			continue
		}
		select {
		case s.messages <- inbound{message: message}:
		default:
			s.terminate()
			return
		}
	}
}

func (s *session) isClosed() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.closed
}

func (s *session) call(ctx context.Context, _ int, method string, params any) (json.RawMessage, error) {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.isClosed() {
		return nil, errors.New("LSP session is closed")
	}
	s.nextID++
	id := s.nextID
	if err := s.write(ctx, rpcOutboundMessage{JSONRPC: "2.0", ID: json.RawMessage(strconvAppendInt(id)), Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("writing LSP %s request: %w", method, err)
	}
	for count := 0; ; count++ {
		if count >= s.config.MaxMessages {
			s.terminate()
			return nil, fmt.Errorf("LSP server sent more than %d messages while handling %s", s.config.MaxMessages, method)
		}
		select {
		case incoming := <-s.messages:
			if incoming.err != nil {
				return nil, fmt.Errorf("reading LSP %s response: %w", method, incoming.err)
			}
			if incoming.message.Method != "" {
				if err := s.handleServerMessage(ctx, incoming.message); err != nil {
					return nil, err
				}
				continue
			}
			if !sameID(incoming.message.ID, id) {
				continue
			}
			if incoming.message.Error != nil {
				return nil, fmt.Errorf("LSP %s failed (%d): %s", method, incoming.message.Error.Code, incoming.message.Error.Message)
			}
			return incoming.message.Result, nil
		case <-ctx.Done():
			s.terminate()
			return nil, fmt.Errorf("LSP %s timed out: %w", method, ctx.Err())
		case <-s.waitDone:
			return nil, fmt.Errorf("LSP server exited while handling %s: %w", method, s.processErr())
		}
	}
}

func (s *session) handleServerMessage(ctx context.Context, message rpcMessage) error {
	if message.Method == "textDocument/publishDiagnostics" && len(message.ID) == 0 {
		var params struct {
			URI         string          `json:"uri"`
			Version     *int            `json:"version"`
			Diagnostics json.RawMessage `json:"diagnostics"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return fmt.Errorf("decoding published diagnostics: %w", err)
		}
		path, ok := confinedURI(s.workspace, params.URI)
		if !ok {
			return nil
		}
		diagnostics, err := parseDiagnostics(params.Diagnostics)
		if err != nil {
			return fmt.Errorf("decoding published diagnostics: %w", err)
		}
		for index := range diagnostics {
			diagnostics[index].FilePath = path
		}
		s.diagnosticsMu.Lock()
		s.diagnostics[path] = diagnostics
		if params.Version != nil {
			s.diagnosticVersions[path] = *params.Version
		} else {
			delete(s.diagnosticVersions, path)
		}
		s.diagnosticsMu.Unlock()
		select {
		case s.diagnosticWake <- struct{}{}:
		default:
		}
		return nil
	}
	if len(message.ID) == 0 {
		return nil
	}
	response := rpcOutboundMessage{JSONRPC: "2.0", ID: message.ID}
	switch message.Method {
	case "workspace/applyEdit":
		response.Result = map[string]any{"applied": false, "failureReason": "client is read-only"}
	case "workspace/configuration":
		response.Result = []any{}
	default:
		response.Error = &rpcError{Code: -32601, Message: "client does not support server request " + message.Method}
	}
	return s.write(ctx, response)
}

func (s *session) notify(ctx context.Context, method string, params any) error {
	s.requestMu.Lock()
	defer s.requestMu.Unlock()
	if s.isClosed() {
		return errors.New("LSP session is closed")
	}
	return s.write(ctx, rpcOutboundMessage{JSONRPC: "2.0", Method: method, Params: params})
}

func (s *session) write(ctx context.Context, message rpcOutboundMessage) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	done := make(chan error, 1)
	go func() {
		done <- writeBoundedRPCMessage(s.stdin, message, s.config.MaxMessageBytes)
	}()
	select {
	case err := <-done:
		if err != nil {
			s.terminate()
		}
		return err
	case <-ctx.Done():
		s.terminate()
		<-done
		return ctx.Err()
	case <-s.waitDone:
		return fmt.Errorf("LSP server exited: %w", s.processErr())
	}
}

func (s *session) terminate() {
	s.closeOnce.Do(func() {
		s.stateMu.Lock()
		s.closed = true
		s.stateMu.Unlock()
		_ = s.stdin.Close()
		_ = s.stdout.Close()
		terminateProcess(s.cmd)
		s.cancel()
	})
}

func (s *session) close() error {
	s.terminate()
	select {
	case <-s.waitDone:
	case <-time.After(time.Second):
		return errors.New("timed out waiting for LSP server to exit")
	}
	return nil
}

func (s *session) processErr() error {
	s.waitMu.Lock()
	defer s.waitMu.Unlock()
	return s.waitErr
}

func strconvAppendInt(value int) []byte {
	return []byte(fmt.Sprintf("%d", value))
}

func sameID(raw json.RawMessage, want int) bool {
	var got int
	return json.Unmarshal(raw, &got) == nil && got == want
}
