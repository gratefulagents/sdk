package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

func TestFakeLSPServer(t *testing.T) {
	if os.Getenv("AGENTSDK_FAKE_LSP_SERVER") != "1" {
		return
	}
	if err := serveFakeLSP(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestGenericToolProtocolUTF16AndConfinement(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "sample.txt")
	if err := os.WriteFile(path, []byte("a😀b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tool := NewTool(fakeConfig())
	defer func() {
		if err := tool.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	}()
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"definition","filePath":"sample.txt","languageId":"plain","line":1,"character":4}`), workspace)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("Execute() result = %#v", result)
	}
	var got Result
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
		t.Fatalf("result is not structured JSON: %v", err)
	}
	if got.Operation != "definition" {
		t.Fatalf("operation = %q, want definition", got.Operation)
	}
	if len(got.Locations) != 1 || got.Locations[0].FilePath != path {
		t.Fatalf("locations = %#v, want only workspace location %q", got.Locations, path)
	}
}

func TestReadOnlyOperationsAndAliases(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewTool(fakeConfig())
	defer tool.Close()

	for _, operation := range []string{
		"goToDefinition", "definition", "findReferences", "references", "hover",
		"documentSymbol", "workspaceSymbol", "implementation", "typeDefinition", "diagnostics",
	} {
		input := fmt.Sprintf(`{"operation":%q,"filePath":"sample.txt","languageId":"plain","line":1,"character":1}`, operation)
		if operation == "workspaceSymbol" {
			input = `{"operation":"workspaceSymbol","languageId":"plain","query":"sample"}`
		}
		result, err := tool.Execute(context.Background(), json.RawMessage(input), workspace)
		if err != nil {
			t.Fatalf("%s Execute() error = %v", operation, err)
		}
		if result.IsError {
			t.Fatalf("%s result = %#v", operation, result)
		}
		var structured Result
		if err := json.Unmarshal([]byte(result.Content), &structured); err != nil {
			t.Fatalf("%s output is not structured JSON: %v", operation, err)
		}
		if structured.Operation == "" {
			t.Fatalf("%s missing operation in %#v", operation, structured)
		}
	}
}

func TestSelectorRejectsMismatchedLanguageAndFile(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.go"), []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := NewTool(fakeConfig())
	defer tool.Close()

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"hover","filePath":"sample.go","languageId":"other","line":1,"character":1}`), workspace)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatalf("Execute() result = %#v, want selector error", result)
	}
}

func TestManagerCloseTerminatesSessions(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "sample.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(fakeConfig())
	if _, err := manager.Execute(context.Background(), workspace, Request{Operation: "hover", FilePath: "sample.txt", LanguageID: "plain", Line: 1, Character: 1}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := manager.Execute(context.Background(), workspace, Request{Operation: "hover", FilePath: "sample.txt", LanguageID: "plain", Line: 1, Character: 1}); err == nil {
		t.Fatal("Execute() after Close() succeeded")
	}
}

type recordingExecutor struct {
	request sandbox.Request
}

func (e *recordingExecutor) Build(_ context.Context, request sandbox.Request) (*exec.Cmd, error) {
	e.request = request
	return nil, fmt.Errorf("stop after recording")
}

func (e *recordingExecutor) Run(context.Context, sandbox.Request) (sandbox.Result, error) {
	return sandbox.Result{}, fmt.Errorf("unexpected Run call")
}

func TestToolCloseIsTerminal(t *testing.T) {
	tool := NewTool(fakeConfig())
	if err := tool.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := tool.Execute(context.Background(), json.RawMessage(`{"operation":"workspaceSymbol","query":"item"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError || !strings.Contains(result.Content, "closed") {
		t.Fatalf("Execute() after Close = %#v", result)
	}
}

func TestZeroCommandPreservesReadOnlySandboxExecutor(t *testing.T) {
	executor := &recordingExecutor{}
	manager := NewManager(Config{Executor: executor})
	_, err := manager.Execute(context.Background(), t.TempDir(), Request{Operation: "workspaceSymbol", Query: "item"})
	if err == nil {
		t.Fatal("Execute() unexpectedly succeeded")
	}
	if len(executor.request.Argv) == 0 || executor.request.Argv[0] != "gopls" {
		t.Fatalf("sandbox argv = %v, want default gopls", executor.request.Argv)
	}
	if executor.request.PermissionMode != policy.PermissionModeReadOnly {
		t.Fatalf("sandbox permission mode = %q, want read-only", executor.request.PermissionMode)
	}
}

func fakeConfig() Config {
	return Config{
		Command:               os.Args[0],
		Args:                  []string{"-test.run=^TestFakeLSPServer$", "--"},
		Env:                   []string{"AGENTSDK_FAKE_LSP_SERVER=1", "AGENTSDK_FAKE_LSP_CONFIG=present"},
		LanguageID:            "plain",
		FilePatterns:          []string{"*.txt"},
		StartupTimeout:        time.Second,
		RequestTimeout:        time.Second,
		MaxMessageBytes:       64 << 10,
		MaxOutputBytes:        64 << 10,
		MaxStderrBytes:        64 << 10,
		MaxMessages:           20,
		AllowUnsafeUnconfined: true,
	}
}

func serveFakeLSP(in *os.File, out *os.File) error {
	if os.Getenv("AGENTSDK_FAKE_LSP_CONFIG") != "present" {
		return fmt.Errorf("configured environment was not passed to LSP server")
	}
	reader := bufio.NewReader(in)
	message, err := readRPCMessage(reader, 64<<10)
	if err != nil {
		return err
	}
	if message.Method != "initialize" {
		return fmt.Errorf("first method = %q, want initialize", message.Method)
	}
	var initialize struct {
		RootURI string `json:"rootUri"`
	}
	if err := json.Unmarshal(message.Params, &initialize); err != nil {
		return err
	}
	if err := writeRPCMessage(out, rpcMessage{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(`{"capabilities":{"diagnosticProvider":true}}`)}); err != nil {
		return err
	}
	message, err = readRPCMessage(reader, 64<<10)
	if err != nil {
		return err
	}
	if message.Method != "initialized" {
		return fmt.Errorf("second method = %q, want initialized", message.Method)
	}

	for {
		message, err = readRPCMessage(reader, 64<<10)
		if err != nil {
			return nil
		}
		if message.Method == "textDocument/didOpen" {
			continue
		}
		if len(message.ID) == 0 {
			continue
		}
		response, err := fakeResponse(message, initialize.RootURI)
		if err != nil {
			response = rpcMessage{JSONRPC: "2.0", ID: message.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}
		}
		if err := writeRPCMessage(out, response); err != nil {
			return err
		}
	}
}

func fakeResponse(message rpcMessage, rootURI string) (rpcMessage, error) {
	if message.Method == "textDocument/definition" {
		var params struct {
			Position struct {
				Line      int `json:"line"`
				Character int `json:"character"`
			} `json:"position"`
		}
		if err := json.Unmarshal(message.Params, &params); err != nil {
			return rpcMessage{}, err
		}
		if params.Position.Line != 0 || (params.Position.Character != 0 && params.Position.Character != 3) {
			return rpcMessage{}, fmt.Errorf("position = %#v, want one-based input mapped to a UTF-16 position", params.Position)
		}
	}

	inside := rootURI + "/sample.txt"
	outside := "file:///outside.txt"
	locationResult := json.RawMessage(fmt.Sprintf(`[{"uri":%q,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}},{"uri":%q,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]`, outside, inside))
	response := rpcMessage{JSONRPC: "2.0", ID: message.ID, Result: locationResult}
	switch message.Method {
	case "textDocument/definition", "textDocument/references", "textDocument/implementation", "textDocument/typeDefinition":
		return response, nil
	case "textDocument/hover":
		response.Result = json.RawMessage(`{"contents":{"kind":"markdown","value":"hover docs"},"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}`)
		return response, nil
	case "textDocument/documentSymbol":
		response.Result = json.RawMessage(`[{"name":"Sample","kind":12,"detail":"detail","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":6}},"selectionRange":{"start":{"line":0,"character":0},"end":{"line":0,"character":6}}}]`)
		return response, nil
	case "workspace/symbol":
		response.Result = json.RawMessage(fmt.Sprintf(`[{"name":"outside","kind":12,"location":{"uri":%q,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}},{"name":"inside","kind":12,"location":{"uri":%q,"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}}]`, outside, inside))
		return response, nil
	case "textDocument/diagnostic":
		response.Result = json.RawMessage(`{"kind":"full","items":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"severity":2,"code":"fake","source":"fake","message":"diagnostic"}]}`)
		return response, nil
	default:
		return rpcMessage{}, fmt.Errorf("unexpected request method %q", message.Method)
	}
}
