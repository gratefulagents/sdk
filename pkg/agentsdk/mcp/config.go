package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// ConfigFileName is the per-repo MCP server configuration file.
	ConfigFileName = ".mcp.json"
)

// Config represents .mcp.json.
type Config struct {
	MCPServers map[string]ServerConfig `json:"mcpServers"`
}

// ServerConfig represents a single MCP server config.
// Only stdio transport is supported right now.
type ServerConfig struct {
	Type              string            `json:"type,omitempty"`
	Command           string            `json:"command,omitempty"`
	Args              []string          `json:"args,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	// AllowEnv is the explicit opt-in list of env names that may pass through
	// the credential denylist (see FilterCredentialEnv). Names listed here are
	// trusted to be set via cfg.Env even if they look like credentials.
	AllowEnv          []string          `json:"allowEnv,omitempty"`
	TrustReadOnlyHint bool              `json:"trustReadOnlyHint,omitempty"`
}

// ConfigPathForWorkDir returns the .mcp.json path for a workspace.
func ConfigPathForWorkDir(workDir string) string {
	if workDir == "" {
		return ConfigFileName
	}
	return filepath.Join(workDir, ConfigFileName)
}

// LoadConfig loads and parses .mcp.json.
//
// The returned exists flag is false when the file is not present.
func LoadConfig(path string) (cfg Config, exists bool, err error) {
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return Config{}, false, nil
		}
		return Config{}, false, fmt.Errorf("read %s: %w", path, readErr)
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, true, fmt.Errorf("parse %s: %w", path, err)
	}

	if cfg.MCPServers == nil {
		cfg.MCPServers = map[string]ServerConfig{}
	}
	return cfg, true, nil
}
