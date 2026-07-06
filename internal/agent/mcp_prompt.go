package agent

import (
	"fmt"
	"strings"
	"unicode"
)

const maxMCPServerNameLen = 64

func buildMCPPromptContext(serverNames []string) string {
	if len(serverNames) == 0 {
		return ""
	}

	sanitized := make([]string, 0, len(serverNames))
	for _, name := range serverNames {
		if s := sanitizeMCPServerName(name); s != "" {
			sanitized = append(sanitized, s)
		}
	}
	if len(sanitized) == 0 {
		return ""
	}

	return fmt.Sprintf(`# MCP Servers

Connected MCP servers: %s

MCP tools are prefixed as mcp__<server>__<tool>.`,
		strings.Join(sanitized, ", "),
	)
}

// sanitizeMCPServerName flattens an MCP-supplied server name to a single
// bounded line so a malicious .mcp.json entry cannot inject instructions into
// the system prompt: control characters are dropped, whitespace runs collapse
// to one space, and the result is capped at maxMCPServerNameLen bytes.
func sanitizeMCPServerName(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	b.Grow(len(name))
	prevSpace := false
	for _, r := range name {
		switch {
		case unicode.IsSpace(r):
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		case unicode.IsControl(r) || !unicode.IsPrint(r):
			continue
		default:
			b.WriteRune(r)
			prevSpace = false
		}
	}
	out := strings.TrimSpace(b.String())
	if runes := []rune(out); len(runes) > maxMCPServerNameLen {
		out = strings.TrimSpace(string(runes[:maxMCPServerNameLen]))
	}
	return out
}
