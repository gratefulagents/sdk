package lsp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

func TestHoverUsesGoplsHoverCommand(t *testing.T) {
	oldRun := runSandboxCommand
	defer func() { runSandboxCommand = oldRun }()

	var argv []string
	runSandboxCommand = func(_ context.Context, req sandbox.Request) (sandbox.Result, error) {
		argv = append([]string(nil), req.Argv...)
		return sandbox.Result{Output: "hover docs", ExitCode: 0}, nil
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := (&Tool{}).hover(context.Background(), input{
		FilePath:  path,
		Line:      1,
		Character: 9,
	}, dir)
	if err != nil {
		t.Fatalf("hover() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("hover() result = %#v, want success", result)
	}
	if len(argv) < 2 || argv[0] != "gopls" || argv[1] != "hover" {
		t.Fatalf("argv = %#v, want gopls hover", argv)
	}
}
