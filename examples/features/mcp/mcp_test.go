package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdkmcp "github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type fakeManager struct {
	args map[string]any
}

func (f *fakeManager) ToolDescriptors() []sdkmcp.ToolDescriptor {
	return []sdkmcp.ToolDescriptor{{
		QualifiedName: "mcp__notes__lookup",
		ServerName:    "notes",
		ToolName:      "lookup",
		Description:   "Lookup notes",
		ReadOnly:      true,
	}}
}

func (f *fakeManager) ConnectedServerNames() []string { return []string{"notes"} }
func (f *fakeManager) HasResources() bool             { return false }
func (f *fakeManager) ListResources(context.Context, string) ([]sdkmcp.ResourceDescriptor, error) {
	return nil, nil
}
func (f *fakeManager) ReadResource(context.Context, string, string) (*mcpsdk.ReadResourceResult, error) {
	return nil, nil
}
func (f *fakeManager) CallTool(_ context.Context, _ string, args map[string]any) (*mcpsdk.CallToolResult, error) {
	f.args = args
	return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "note found"}}}, nil
}

func TestMCPToolBuilderExample(t *testing.T) {
	manager := &fakeManager{}
	tools := sdkmcp.BuildTools(manager)
	if len(tools) != 1 {
		t.Fatalf("BuildTools() length = %d, want 1", len(tools))
	}

	result, err := tools[0].Execute(context.Background(), json.RawMessage(`{"query":"sdk"}`), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "note found" || manager.args["query"] != "sdk" {
		t.Fatalf("result=%+v args=%#v", result, manager.args)
	}
}

// TestMCPConfigSnapshotAndSanitizeExample exercises the real public MCP API:
// LoadConfigSnapshot pins the file's content hash; VerifyUnchanged refuses a
// silent reload if the file has been mutated mid-run; SanitizeMCPDisplay
// strips control characters from server-supplied display text.
func TestMCPConfigSnapshotAndSanitizeExample(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, sdkmcp.ConfigFileName)

	cfg := sdkmcp.Config{
		MCPServers: map[string]sdkmcp.ServerConfig{
			"echo": {
				Type:    "stdio",
				Command: "/bin/sh",
				Args:    []string{"-c", "echo test"},
			},
		},
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := sdkmcp.LoadConfigSnapshot(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfigSnapshot: %v", err)
	}
	if len(snap.ContentSHA256) == 0 {
		t.Fatal("expected non-empty content hash on a present .mcp.json")
	}
	if _, ok := snap.Config.MCPServers["echo"]; !ok {
		t.Fatalf("snapshot missing echo server: %+v", snap.Config)
	}
	if snap.AbsPath == "" || !filepath.IsAbs(snap.AbsPath) {
		t.Fatalf("AbsPath = %q, want absolute path", snap.AbsPath)
	}

	// Snapshot is honoured: unchanged file passes verification.
	if err := snap.VerifyUnchanged(); err != nil {
		t.Fatalf("VerifyUnchanged on unchanged file: %v", err)
	}

	// Mutate the on-disk file and verify the snapshot refuses the reload.
	mutated := append(cfgBytes, []byte("\n// trailing edit\n")...)
	if err := os.WriteFile(cfgPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := snap.VerifyUnchanged(); err == nil {
		t.Fatal("VerifyUnchanged on mutated file: expected error, got nil")
	} else if !strings.Contains(err.Error(), "modified") {
		t.Fatalf("VerifyUnchanged error = %q, want to mention modification", err.Error())
	}

	// LoadConfig is the lower-level public entry; round-trip should still parse
	// the (mutated, but JSON-valid bytes were written first as a comment which
	// breaks JSON — so use the original bytes).
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, exists, err := sdkmcp.LoadConfig(cfgPath)
	if err != nil || !exists {
		t.Fatalf("LoadConfig: err=%v exists=%v", err, exists)
	}
	if parsed.MCPServers["echo"].Command != "/bin/sh" {
		t.Fatalf("LoadConfig round-trip lost data: %+v", parsed)
	}

	// SanitizeMCPDisplay must strip control chars / ANSI escapes and prepend
	// a provenance tag so approval UIs can't be tricked by server-supplied
	// text. \x1b[31m is an ANSI red escape; \x07 is BEL; \x00 is NUL.
	out := sdkmcp.SanitizeMCPDisplay("evil-server", "click \x1b[31mHERE\x1b[0m\x07 to win\x00")
	if !strings.HasPrefix(out, "[from MCP server: evil-server]") {
		t.Fatalf("missing provenance tag: %q", out)
	}
	for _, bad := range []string{"\x1b", "\x07", "\x00"} {
		if strings.Contains(out, bad) {
			t.Fatalf("sanitized output still contains control char %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "click") || !strings.Contains(out, "HERE") || !strings.Contains(out, "to win") {
		t.Fatalf("sanitized output dropped legitimate text: %q", out)
	}

	// Provenance tag should also sanitize the server name itself.
	out2 := sdkmcp.SanitizeMCPDisplay("evil\x1bname", "ok")
	if strings.Contains(out2, "\x1b") {
		t.Fatalf("server name not sanitized in tag: %q", out2)
	}
}
