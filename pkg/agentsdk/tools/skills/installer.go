package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

// Installer manages skill installation in workspaces via .mcp.json.
type Installer struct {
	registry *Registry
}

// NewInstaller creates an installer backed by the given registry.
func NewInstaller(registry *Registry) *Installer {
	return &Installer{registry: registry}
}

// Install adds the named skill's MCP server config to the workspace's .mcp.json.
func (inst *Installer) Install(workDir, skillName string) error {
	if inst == nil || inst.registry == nil {
		return fmt.Errorf("skill installer registry is not configured")
	}
	skill, ok := inst.registry.Get(skillName)
	if !ok {
		return fmt.Errorf("skill %q not found in registry", skillName)
	}

	cfgPath, err := mcpConfigPath(workDir)
	if err != nil {
		return err
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading .mcp.json: %w", err)
	}

	cfg.MCPServers[skill.Name] = mcp.ServerConfig{
		Type:    skill.MCPConfig.Type,
		Command: skill.MCPConfig.Command,
		Args:    skill.MCPConfig.Args,
		Env:     skill.MCPConfig.Env,
	}

	return saveMCPConfig(cfgPath, cfg)
}

// Uninstall removes the named skill's MCP server config from .mcp.json.
func (inst *Installer) Uninstall(workDir, skillName string) error {
	if inst == nil || inst.registry == nil {
		return fmt.Errorf("skill installer registry is not configured")
	}
	cfgPath, err := mcpConfigPath(workDir)
	if err != nil {
		return err
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading .mcp.json: %w", err)
	}

	if _, exists := cfg.MCPServers[skillName]; !exists {
		return fmt.Errorf("skill %q is not installed", skillName)
	}

	delete(cfg.MCPServers, skillName)

	if len(cfg.MCPServers) == 0 {
		return os.Remove(cfgPath)
	}
	return saveMCPConfig(cfgPath, cfg)
}

// ListInstalled returns the names of skills currently configured in .mcp.json.
func (inst *Installer) ListInstalled(workDir string) ([]string, error) {
	if inst == nil || inst.registry == nil {
		return nil, fmt.Errorf("skill installer registry is not configured")
	}
	cfgPath, err := mcpConfigPath(workDir)
	if err != nil {
		return nil, err
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	return names, nil
}

func mcpConfigPath(workDir string) (string, error) {
	return pathutil.ResolveWorkspace(workDir, ".mcp.json")
}

func loadMCPConfig(path string) (*mcp.Config, error) {
	cfg, exists, err := mcp.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		cfg.MCPServers = make(map[string]mcp.ServerConfig)
	}
	return &cfg, nil
}

func saveMCPConfig(path string, cfg *mcp.Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".mcp.json.tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	cleanup = false
	return nil
}
