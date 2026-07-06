package mcpcatalog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
)

// mustNewSkillRegistry returns a registry with fixture entries. The SDK ships
// an empty default catalog (hosts supply their own entries via
// NewRegistryFromEntries), so behavior tests use fixtures.
func mustNewSkillRegistry(t *testing.T) *Registry {
	t.Helper()
	return NewRegistryFromEntries([]CatalogEntry{
		{
			Name:        "search-duckduckgo",
			Description: "Web search via DuckDuckGo - no API key required",
			Category:    "search",
			Version:     "0.1.0",
			MCPConfig:   MCPServerConfig{Type: "stdio", Command: "uvx", Args: []string{"duckduckgo-mcp-server"}},
			Tags:        []string{"search", "web", "free"},
			Verified:    true,
		},
		{
			Name:            "search-exa",
			Description:     "AI-powered web search via Exa",
			Category:        "search",
			Version:         "0.1.0",
			MCPConfig:       MCPServerConfig{Type: "stdio", Command: "npx", Args: []string{"-y", "exa-mcp-server"}},
			Tags:            []string{"search", "web", "ai"},
			Verified:        true,
			RequiresEnvVars: []string{"EXA_API_KEY"},
		},
	})
}

// TestLoadDefaultCatalog pins the contract that the SDK ships no default MCP
// servers: the embedded catalog must stay empty. Hosts that want an
// installable catalog provide their own entries via NewRegistryFromEntries.
func TestLoadDefaultCatalog(t *testing.T) {
	skills, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog() error = %v", err)
	}
	if len(skills) != 0 {
		t.Fatalf("default catalog must be empty, got %d entries", len(skills))
	}
}

func TestSkillSearchToolSearchByQuery(t *testing.T) {
	tool := &SearchTool{Registry: mustNewSkillRegistry(t)}
	input, _ := json.Marshal(map[string]string{"query": "duckduckgo"})

	result, err := tool.Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "search-duckduckgo") {
		t.Fatalf("result = %+v", result)
	}
}

func TestSkillSearchToolSearchByCategory(t *testing.T) {
	tool := &SearchTool{Registry: mustNewSkillRegistry(t)}
	input, _ := json.Marshal(map[string]string{"category": "search"})

	result, err := tool.Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError || !strings.Contains(result.Content, "Found") {
		t.Fatalf("result = %+v", result)
	}
}

func TestSkillSearchToolNoResults(t *testing.T) {
	tool := &SearchTool{Registry: mustNewSkillRegistry(t)}
	input, _ := json.Marshal(map[string]string{"query": "nonexistent-xyz-tool"})

	result, err := tool.Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "No skills found") {
		t.Fatalf("result = %+v", result)
	}
}

func TestSkillInstallTool(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	dir := t.TempDir()
	tool := &InstallTool{Installer: NewInstaller(registry), WorkDir: dir}
	input, _ := json.Marshal(map[string]string{"name": "search-duckduckgo"})

	result, err := tool.Execute(context.Background(), input, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("expected .mcp.json to exist: %v", err)
	}
	if !strings.Contains(string(data), "search-duckduckgo") {
		t.Fatalf("expected search-duckduckgo in .mcp.json, got: %s", string(data))
	}
}

func TestSkillInstallRejectsSymlinkMCPConfigEscape(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	root := t.TempDir()
	dir := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside.mcp.json")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(`{"mcpServers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".mcp.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	err := NewInstaller(registry).Install(dir, "search-duckduckgo")
	if err == nil || !strings.Contains(err.Error(), "outside the workspace root") {
		t.Fatalf("Install() error = %v, want workspace escape", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"mcpServers":{}}` {
		t.Fatalf("outside config changed: %s", data)
	}
}

func TestSkillInstallToolNotFound(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	tool := &InstallTool{Installer: NewInstaller(registry), WorkDir: t.TempDir()}
	input, _ := json.Marshal(map[string]string{"name": "nonexistent"})

	result, err := tool.Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !result.IsError {
		t.Fatal("expected error for nonexistent skill")
	}
}

func TestSkillListInstalledTool(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	installer := NewInstaller(registry)
	dir := t.TempDir()

	emptyTool := &ListInstalledTool{Installer: installer, WorkDir: dir}
	empty, err := emptyTool.Execute(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(empty.Content, "No skills") {
		t.Fatalf("empty result = %+v", empty)
	}

	if err := installer.Install(dir, "search-duckduckgo"); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	result, err := emptyTool.Execute(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "Installed skills: search-duckduckgo") {
		t.Fatalf("result = %+v", result)
	}
	if strings.Contains(result.Content, "Other MCP servers") {
		t.Fatalf("catalog-only install should not list other servers: %+v", result)
	}
}

func TestSkillListInstalledToolSeparatesNonCatalogServers(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	installer := NewInstaller(registry)
	dir := t.TempDir()
	tool := &ListInstalledTool{Installer: installer, WorkDir: dir}

	if err := installer.Install(dir, "search-duckduckgo"); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	cfgPath, err := mcpConfigPath(dir)
	if err != nil {
		t.Fatalf("mcpConfigPath() error = %v", err)
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		t.Fatalf("loadMCPConfig() error = %v", err)
	}
	cfg.MCPServers["custom-server"] = mcp.ServerConfig{Type: "stdio", Command: "custom-mcp"}
	if err := saveMCPConfig(cfgPath, cfg); err != nil {
		t.Fatalf("saveMCPConfig() error = %v", err)
	}

	result, err := tool.Execute(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "Installed skills: search-duckduckgo") {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(result.Content, "Other MCP servers in .mcp.json (not from the skill catalog): custom-server") {
		t.Fatalf("result missing non-catalog server line: %+v", result)
	}

	if err := installer.Uninstall(dir, "search-duckduckgo"); err != nil {
		t.Fatalf("Uninstall() error = %v", err)
	}
	onlyOther, err := tool.Execute(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(onlyOther.Content, "No skills from the catalog are installed") {
		t.Fatalf("result = %+v", onlyOther)
	}
	if !strings.Contains(onlyOther.Content, "custom-server") {
		t.Fatalf("result missing non-catalog server: %+v", onlyOther)
	}
}
