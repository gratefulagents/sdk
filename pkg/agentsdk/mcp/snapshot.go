package mcp

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConfigSnapshot is an immutable view of an .mcp.json load taken at a single
// instant. The snapshot pins the absolute file path and a SHA-256 of the file
// content; callers must NOT silently reload the file mid-run. Use
// VerifyUnchanged to detect tampering before re-reading.
type ConfigSnapshot struct {
	// Path is the path passed to LoadConfigSnapshot, as supplied by the caller.
	Path string
	// AbsPath is the resolved absolute path to .mcp.json. Pinned at load time.
	AbsPath string
	// ContentSHA256 is the digest of the file bytes at load time.
	ContentSHA256 []byte
	// LoadedAt is the wall-clock time when the snapshot was taken.
	LoadedAt time.Time
	// Config is the parsed configuration.
	Config Config
	// AgentWritable is true when the .mcp.json file resides inside the
	// agent-writable workspace and is therefore subject to in-run mutation. The
	// caller has been warned via the standard logger.
	AgentWritable bool
}

// LoadConfigSnapshot reads and parses .mcp.json, pinning the absolute path and
// content hash. The returned snapshot is immutable; callers must treat
// .mcp.json as immutable for the rest of the run.
//
// If the file does not exist, the returned snapshot has an empty Config and
// nil ContentSHA256, but AbsPath is still pinned.
func LoadConfigSnapshot(path string) (ConfigSnapshot, error) {
	return loadConfigSnapshot(path, "")
}

// LoadConfigSnapshotInWorkspace is like LoadConfigSnapshot but additionally
// flags the snapshot when the resolved .mcp.json path lives inside the
// agent-writable workspace. A warning is also logged in that case.
func LoadConfigSnapshotInWorkspace(path, workspaceRoot string) (ConfigSnapshot, error) {
	return loadConfigSnapshot(path, workspaceRoot)
}

func loadConfigSnapshot(path, workspaceRoot string) (ConfigSnapshot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return ConfigSnapshot{}, fmt.Errorf("resolve %s: %w", path, err)
	}
	snap := ConfigSnapshot{
		Path:     path,
		AbsPath:  abs,
		LoadedAt: time.Now(),
	}

	if workspaceRoot != "" {
		absRoot, absErr := filepath.Abs(workspaceRoot)
		if absErr == nil && pathIsWithin(abs, absRoot) {
			snap.AgentWritable = true
			log.Printf("mcp: WARNING .mcp.json at %s is inside agent-writable workspace %s; "+
				"contents are pinned at load time and will not be silently reloaded",
				abs, absRoot)
		}
	}

	data, readErr := os.ReadFile(abs)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			snap.Config = Config{MCPServers: map[string]ServerConfig{}}
			return snap, nil
		}
		return snap, fmt.Errorf("read %s: %w", abs, readErr)
	}

	sum := sha256.Sum256(data)
	snap.ContentSHA256 = sum[:]

	cfg, _, parseErr := LoadConfig(abs)
	if parseErr != nil {
		return snap, parseErr
	}
	snap.Config = cfg
	return snap, nil
}

// VerifyUnchanged re-reads the pinned file and returns an error if the bytes
// have changed since the snapshot was taken. Use this before honouring any
// caller-requested reload of .mcp.json — silent reloads are refused; the
// caller must explicitly re-snapshot if they truly intend to swap config.
func (s ConfigSnapshot) VerifyUnchanged() error {
	if s.AbsPath == "" {
		return errors.New("snapshot has no pinned path")
	}
	data, err := os.ReadFile(s.AbsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if len(s.ContentSHA256) == 0 {
				return nil
			}
			return fmt.Errorf("MCP config %s was modified (deleted) since snapshot at %s",
				s.AbsPath, s.LoadedAt.Format(time.RFC3339))
		}
		return err
	}
	sum := sha256.Sum256(data)
	if !bytes.Equal(sum[:], s.ContentSHA256) {
		return fmt.Errorf("MCP config %s was modified since snapshot at %s; "+
			"silent reload refused — restart the agent to pick up new servers",
			s.AbsPath, s.LoadedAt.Format(time.RFC3339))
	}
	return nil
}

func pathIsWithin(child, parent string) bool {
	if parent == "" {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..")
}
