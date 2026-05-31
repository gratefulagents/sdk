package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustNewSkillRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry()
	if err != nil {
		t.Fatalf("failed to create skill registry: %v", err)
	}
	return r
}

func TestLoadDefaultCatalog(t *testing.T) {
	skills, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog() error = %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("expected non-empty catalog")
	}
	for _, skill := range skills {
		if skill.Name == "" || skill.Description == "" || skill.Category == "" || skill.MCPConfig.Command == "" {
			t.Fatalf("skill missing required fields: %+v", skill)
		}
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
	if !strings.Contains(result.Content, "search-duckduckgo") {
		t.Fatalf("result = %+v", result)
	}
}
