package mcpcatalog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gratefulagents/sdk/pkg/agentsdk/mcp"
	"github.com/gratefulagents/sdk/pkg/agentsdk/tools/internal/pathutil"
)

// Installer manages catalog entry installation in workspaces via .mcp.json.
type Installer struct {
	registry *Registry
}

// NewInstaller creates an installer backed by the given registry.
func NewInstaller(registry *Registry) *Installer {
	return &Installer{registry: registry}
}

// Entry returns the catalog entry for name from the installer's registry.
func (inst *Installer) Entry(name string) (*CatalogEntry, bool) {
	if inst == nil || inst.registry == nil {
		return nil, false
	}
	return inst.registry.Get(name)
}

// Install adds the named entry's MCP server config to the workspace's .mcp.json.
//
// The entry's allowEnv is seeded from its requiresEnvVars so that
// credential-looking variables the server genuinely needs pass the SDK's
// credential env filter once the host supplies them (via the entry's env map
// or a host-level secret mechanism). If an entry with the same name already
// exists, its hardening fields (allowEnv, allowedTools, enabled, env) are
// preserved and merged rather than silently clobbered.
func (inst *Installer) Install(workDir, entryName string) error {
	if inst == nil || inst.registry == nil {
		return fmt.Errorf("catalog installer registry is not configured")
	}
	ce, ok := inst.registry.Get(entryName)
	if !ok {
		return fmt.Errorf("catalog entry %q not found in registry", entryName)
	}

	cfgPath, err := mcpConfigPath(workDir)
	if err != nil {
		return err
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading .mcp.json: %w", err)
	}

	entry := mcp.ServerConfig{
		Type:     ce.MCPConfig.Type,
		Command:  ce.MCPConfig.Command,
		Args:     ce.MCPConfig.Args,
		Env:      cloneStringMap(ce.MCPConfig.Env),
		AllowEnv: append([]string(nil), ce.RequiresEnvVars...),
	}
	if existing, ok := cfg.MCPServers[ce.Name]; ok {
		// Reinstall/update: keep user- or platform-applied hardening.
		entry.AllowEnv = mergeUnique(existing.AllowEnv, entry.AllowEnv)
		entry.AllowedTools = existing.AllowedTools
		entry.Enabled = existing.Enabled
		for k, v := range existing.Env {
			if entry.Env == nil {
				entry.Env = make(map[string]string)
			}
			if _, exists := entry.Env[k]; !exists {
				entry.Env[k] = v
			}
		}
	}
	cfg.MCPServers[ce.Name] = entry

	return saveMCPConfig(cfgPath, cfg)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeUnique(lists ...[]string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, list := range lists {
		for _, v := range list {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Uninstall removes the named entry's MCP server config from .mcp.json.
func (inst *Installer) Uninstall(workDir, entryName string) error {
	if inst == nil || inst.registry == nil {
		return fmt.Errorf("catalog installer registry is not configured")
	}
	cfgPath, err := mcpConfigPath(workDir)
	if err != nil {
		return err
	}
	cfg, err := loadMCPConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading .mcp.json: %w", err)
	}

	if _, exists := cfg.MCPServers[entryName]; !exists {
		return fmt.Errorf("catalog entry %q is not installed", entryName)
	}

	delete(cfg.MCPServers, entryName)

	if len(cfg.MCPServers) == 0 {
		return os.Remove(cfgPath)
	}
	return saveMCPConfig(cfgPath, cfg)
}

// ListInstalled returns the names of MCP servers currently configured in .mcp.json.
func (inst *Installer) ListInstalled(workDir string) ([]string, error) {
	if inst == nil || inst.registry == nil {
		return nil, fmt.Errorf("catalog installer registry is not configured")
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
