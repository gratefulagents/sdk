package tools_registry_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/policy"
	sdktools "github.com/gratefulagents/sdk/pkg/agentsdk/tools"
)

func TestSDKToolRegistryExample(t *testing.T) {
	workDir := t.TempDir()
	registry := sdktools.NewRegistry(workDir, sdktools.WithPermissionMode(policy.PermissionModeWorkspaceWrite))

	write := registry.Get("Write")
	if write == nil {
		t.Fatal("Write tool missing")
	}
	if !write.IsEnabled(&agentsdk.RunContext{ToolAccessLevel: agentsdk.ToolAccessLevelFull}) {
		t.Fatal("Write should be enabled for full access")
	}

	result, err := write.Execute(context.Background(), json.RawMessage(`{"file_path":"notes.txt","content":"hello"}`), workDir)
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError {
		t.Fatalf("Write returned error: %s", result.Content)
	}
	data, err := os.ReadFile(filepath.Join(workDir, "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("file content = %q, want hello", string(data))
	}
}
