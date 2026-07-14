package mcp

import (
	"context"
	"os"
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
