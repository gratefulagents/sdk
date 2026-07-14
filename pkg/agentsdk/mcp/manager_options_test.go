package mcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	"github.com/gratefulagents/sdk/pkg/agentsdk/sandbox"
)

func TestResolveManagerOptionsTrustsManagerWorkDir(t *testing.T) {
	binDir := t.TempDir()
	bwrap := filepath.Join(binDir, "bwrap")
	if err := os.WriteFile(bwrap, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	t.Setenv(sandbox.SandboxModeEnv, "required")

	for _, tt := range []struct {
		name string
		opts []ManagerOption
	}{
		{name: "environment defaults"},
		{
			name: "host sandbox config",
			opts: []ManagerOption{WithCommandSandboxConfig(sandbox.Config{
				Mode:          "required",
				WorkspaceRoot: filepath.Join(t.TempDir(), "untrusted"),
			})},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workDir := t.TempDir()
			options := resolveManagerOptions(workDir, tt.opts...)
			cmd, err := options.executor.Build(context.Background(), sandbox.Request{
				Argv:           []string{"/bin/true"},
				WorkDir:        workDir,
				PermissionMode: policy.PermissionModeWorkspaceWrite,
			})
			if err != nil {
				t.Fatalf("build restricted MCP command: %v", err)
			}
			if !containsArgSequence(cmd.Args, "--bind", workDir, workDir) {
				t.Fatalf("sandbox args do not bind trusted workDir %q: %v", workDir, cmd.Args)
			}
		})
	}
}

func containsArgSequence(args []string, want ...string) bool {
	for i := 0; i+len(want) <= len(args); i++ {
		match := true
		for j := range want {
			if args[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

type recordingExecutor struct {
	requests []sandbox.Request
}

func (e *recordingExecutor) Build(_ context.Context, req sandbox.Request) (*exec.Cmd, error) {
	e.requests = append(e.requests, req)
	return nil, errors.New("recorded request")
}

func (*recordingExecutor) Run(context.Context, sandbox.Request) (sandbox.Result, error) {
	return sandbox.Result{}, nil
}

func TestConnectStdioServerNetworkAccessAllowlist(t *testing.T) {
	tests := []struct {
		name    string
		opts    []ManagerOption
		server  string
		allowed bool
	}{
		{
			name:    "allowlisted normalized name",
			opts:    []ManagerOption{WithNetworkAccessForServers(" opted ")},
			server:  "opted",
			allowed: true,
		},
		{
			name:    "unlisted server",
			opts:    []ManagerOption{WithNetworkAccessForServers("opted")},
			server:  "unlisted",
			allowed: false,
		},
		{
			name:    "default",
			server:  "default",
			allowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			executor := &recordingExecutor{}
			opts := resolveManagerOptions(t.TempDir(), append([]ManagerOption{WithCommandExecutor(executor)}, tt.opts...)...)
			_, err := connectStdioServer(context.Background(), t.TempDir(), tt.server, ServerConfig{Command: "server"}, opts)
			if err == nil {
				t.Fatal("connectStdioServer() error = nil, want executor error")
			}
			if len(executor.requests) != 1 {
				t.Fatalf("executor requests = %d, want 1", len(executor.requests))
			}
			if got := executor.requests[0].AllowNetwork; got != tt.allowed {
				t.Errorf("Request.AllowNetwork = %v, want %v", got, tt.allowed)
			}
		})
	}
}
