package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
)

// TestDefaultCatalogHygiene guards the invariants that made past catalog
// entries silently broken: ${VAR} placeholders in env/args never expand to
// parent-process values (the SDK expands only against the safe base env), and
// credential-looking env keys are stripped unless listed in allowEnv. Catalog
// entries must therefore declare credentials via requiresEnvVars only.
func TestDefaultCatalogHygiene(t *testing.T) {
	skills, err := LoadDefaultCatalog()
	if err != nil {
		t.Fatalf("LoadDefaultCatalog() error = %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("expected non-empty catalog")
	}
	seen := make(map[string]bool)
	for _, skill := range skills {
		if seen[skill.Name] {
			t.Errorf("duplicate skill name %q", skill.Name)
		}
		seen[skill.Name] = true

		if skill.MCPConfig.Type != "" && !strings.EqualFold(skill.MCPConfig.Type, "stdio") {
			t.Errorf("skill %q: unsupported transport %q (SDK is stdio-only)", skill.Name, skill.MCPConfig.Type)
		}
		if cmd := skill.MCPConfig.Command; cmd != "npx" && cmd != "uvx" {
			t.Errorf("skill %q: unexpected command %q (worker images preinstall npx/uvx only)", skill.Name, cmd)
		}
		for _, arg := range skill.MCPConfig.Args {
			if strings.Contains(arg, "${") {
				t.Errorf("skill %q: arg %q contains a ${VAR} placeholder that would expand to empty", skill.Name, arg)
			}
		}
		for k, v := range skill.MCPConfig.Env {
			if strings.Contains(v, "${") {
				t.Errorf("skill %q: env %s=%q contains a ${VAR} placeholder that would expand to empty; declare it in requiresEnvVars instead", skill.Name, k, v)
			}
		}
		for _, name := range skill.RequiresEnvVars {
			if strings.TrimSpace(name) == "" {
				t.Errorf("skill %q: empty requiresEnvVars entry", skill.Name)
			}
		}
	}
}

func readInstalledConfig(t *testing.T, dir string) map[string]mcp.ServerConfig {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".mcp.json"))
	if err != nil {
		t.Fatalf("reading .mcp.json: %v", err)
	}
	var cfg struct {
		MCPServers map[string]mcp.ServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parsing .mcp.json: %v", err)
	}
	return cfg.MCPServers
}

func TestSkillInstallSeedsAllowEnvFromRequiresEnvVars(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	dir := t.TempDir()
	if err := NewInstaller(registry).Install(dir, "search-exa"); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	entry, ok := readInstalledConfig(t, dir)["search-exa"]
	if !ok {
		t.Fatal("search-exa missing from .mcp.json")
	}
	if len(entry.AllowEnv) != 1 || entry.AllowEnv[0] != "EXA_API_KEY" {
		t.Fatalf("allowEnv = %v, want [EXA_API_KEY]", entry.AllowEnv)
	}
}

func TestSkillInstallPreservesExistingHardening(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	dir := t.TempDir()
	disabled := false
	existing := map[string]mcp.ServerConfig{
		"search-exa": {
			Command:      "npx",
			Args:         []string{"-y", "old-package"},
			Env:          map[string]string{"EXA_BASE_URL": "https://example.test"},
			AllowEnv:     []string{"CUSTOM_TOKEN"},
			AllowedTools: []string{"web_search"},
			Enabled:      &disabled,
		},
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": existing})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".mcp.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := NewInstaller(registry).Install(dir, "search-exa"); err != nil {
		t.Fatalf("Install() error = %v", err)
	}
	entry := readInstalledConfig(t, dir)["search-exa"]

	if got := strings.Join(entry.Args, " "); !strings.Contains(got, "exa-mcp-server") {
		t.Fatalf("args not updated from catalog: %v", entry.Args)
	}
	if entry.Env["EXA_BASE_URL"] != "https://example.test" {
		t.Fatalf("existing env dropped: %v", entry.Env)
	}
	allow := strings.Join(entry.AllowEnv, ",")
	if !strings.Contains(allow, "CUSTOM_TOKEN") || !strings.Contains(allow, "EXA_API_KEY") {
		t.Fatalf("allowEnv not merged: %v", entry.AllowEnv)
	}
	if len(entry.AllowedTools) != 1 || entry.AllowedTools[0] != "web_search" {
		t.Fatalf("allowedTools dropped: %v", entry.AllowedTools)
	}
	if entry.Enabled == nil || *entry.Enabled {
		t.Fatalf("enabled=false dropped: %v", entry.Enabled)
	}
}

func TestSkillInstallToolWarnsAboutRestartAndMissingEnv(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	dir := t.TempDir()
	tool := &InstallTool{Installer: NewInstaller(registry), WorkDir: dir}
	t.Setenv("EXA_API_KEY", "")
	input, _ := json.Marshal(map[string]string{"name": "search-exa"})

	result, err := tool.Execute(context.Background(), input, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if !strings.Contains(result.Content, "after the agent restarts") {
		t.Fatalf("result does not state restart semantics: %s", result.Content)
	}
	if !strings.Contains(result.Content, "WARNING") || !strings.Contains(result.Content, "EXA_API_KEY") {
		t.Fatalf("result does not warn about missing EXA_API_KEY: %s", result.Content)
	}
}

func TestSkillInstallToolNoWarningWhenEnvPresent(t *testing.T) {
	registry := mustNewSkillRegistry(t)
	dir := t.TempDir()
	tool := &InstallTool{Installer: NewInstaller(registry), WorkDir: dir}
	t.Setenv("EXA_API_KEY", "test-key")
	input, _ := json.Marshal(map[string]string{"name": "search-exa"})

	result, err := tool.Execute(context.Background(), input, dir)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if strings.Contains(result.Content, "WARNING") {
		t.Fatalf("unexpected warning with env present: %s", result.Content)
	}
}

func TestSkillSearchShowsRequiredEnvVars(t *testing.T) {
	tool := &SearchTool{Registry: mustNewSkillRegistry(t)}
	t.Setenv("EXA_API_KEY", "")
	input, _ := json.Marshal(map[string]string{"query": "exa"})

	result, err := tool.Execute(context.Background(), input, "")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(result.Content, "Requires env: EXA_API_KEY (NOT set)") {
		t.Fatalf("search output missing env requirement: %s", result.Content)
	}
}
