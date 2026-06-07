package mcp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_FileMissing(t *testing.T) {
	t.Parallel()

	cfg, exists, err := LoadConfig(filepath.Join(t.TempDir(), ".mcp.json"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if exists {
		t.Fatalf("exists = true, want false")
	}
	if len(cfg.MCPServers) != 0 {
		t.Fatalf("MCPServers len = %d, want 0", len(cfg.MCPServers))
	}
}

func TestLoadConfig_Parse(t *testing.T) {
	t.Parallel()

	disabled := false
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	content := `{
	  "mcpServers": {
	    "github": {
	      "command": "npx",
	      "args": ["-y", "@modelcontextprotocol/server-github"],
	      "enabled": false,
	      "trustReadOnlyHint": true,
	      "allowedTools": ["get_issue", "list_prs"]
	    }
	  }
	}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, exists, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !exists {
		t.Fatalf("exists = false, want true")
	}

	githubCfg, ok := cfg.MCPServers["github"]
	if !ok {
		t.Fatalf("github server not found in config")
	}
	if githubCfg.Command != "npx" {
		t.Fatalf("command = %q, want npx", githubCfg.Command)
	}
	if len(githubCfg.Args) != 2 {
		t.Fatalf("args len = %d, want 2", len(githubCfg.Args))
	}
	if !githubCfg.TrustReadOnlyHint {
		t.Fatalf("trustReadOnlyHint = false, want true")
	}
	if githubCfg.Enabled == nil || *githubCfg.Enabled != disabled {
		t.Fatalf("enabled = %v, want false", githubCfg.Enabled)
	}
	if len(githubCfg.AllowedTools) != 2 || githubCfg.AllowedTools[0] != "get_issue" || githubCfg.AllowedTools[1] != "list_prs" {
		t.Fatalf("AllowedTools = %#v", githubCfg.AllowedTools)
	}
	if serverToolAllowed(githubCfg, "create_issue") {
		t.Fatal("serverToolAllowed(create_issue) = true, want false")
	}
	if !serverToolAllowed(githubCfg, "get_issue") {
		t.Fatal("serverToolAllowed(get_issue) = false, want true")
	}
}
