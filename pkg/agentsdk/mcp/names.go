package mcp

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

const maxMCPToolNameLen = 64

var invalidMCPNameChars = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// NormalizeNameForMCP normalizes server/tool names to satisfy API tool-name constraints.
func NormalizeNameForMCP(name string) string {
	normalized := invalidMCPNameChars.ReplaceAllString(strings.TrimSpace(name), "_")
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		return "unnamed"
	}
	return normalized
}

// BuildToolName builds an MCP-qualified tool name:
// mcp__<normalized-server>__<normalized-tool>
//
// The final name is truncated and hashed when needed to fit the 64-char limit.
func BuildToolName(serverName, toolName string) string {
	server := NormalizeNameForMCP(serverName)
	tool := NormalizeNameForMCP(toolName)
	base := fmt.Sprintf("mcp__%s__%s", server, tool)
	if len(base) <= maxMCPToolNameLen {
		return base
	}

	hash := shortHash(base)

	// Format: mcp__<server>__<tool>_<hash8>
	const fixedLen = len("mcp__") + len("__") + len("_") + 8
	budget := maxMCPToolNameLen - fixedLen
	if budget < 2 {
		return "mcp__" + hash
	}

	serverBudget := budget / 2
	toolBudget := budget - serverBudget

	if len(server) < serverBudget {
		toolBudget += serverBudget - len(server)
		serverBudget = len(server)
	}
	if len(tool) < toolBudget {
		serverBudget += toolBudget - len(tool)
		toolBudget = len(tool)
	}
	if serverBudget < 1 {
		serverBudget = 1
	}
	if toolBudget < 1 {
		toolBudget = 1
	}

	server = truncateASCII(server, serverBudget)
	tool = truncateASCII(tool, toolBudget)
	return fmt.Sprintf("mcp__%s__%s_%s", server, tool, hash)
}

// EnsureUniqueToolName ensures the generated tool name is unique in this registry.
// Caps collision search at 1000 attempts to avoid an unbounded loop on
// pathological configs; falls back to appending a hash of the iteration count.
func EnsureUniqueToolName(candidate string, used map[string]struct{}) string {
	if _, ok := used[candidate]; !ok {
		used[candidate] = struct{}{}
		return candidate
	}

	for i := 2; i < 1002; i++ {
		suffix := fmt.Sprintf("_%d", i)
		name := candidate
		if len(name)+len(suffix) > maxMCPToolNameLen {
			name = truncateASCII(name, maxMCPToolNameLen-len(suffix))
		}
		name += suffix
		if _, ok := used[name]; !ok {
			used[name] = struct{}{}
			return name
		}
	}
	// Fallback: append a hash of the candidate to guarantee uniqueness without
	// further iteration. Collisions on the hash itself are astronomically
	// unlikely for the inputs this function ever sees.
	suffix := "_" + shortHash(fmt.Sprintf("%s_%d", candidate, len(used)))
	name := candidate
	if len(name)+len(suffix) > maxMCPToolNameLen {
		name = truncateASCII(name, maxMCPToolNameLen-len(suffix))
	}
	name += suffix
	used[name] = struct{}{}
	return name
}

func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:4])
}

func truncateASCII(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	return s[:n]
}
