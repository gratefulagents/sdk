package agent

import (
	"fmt"
	"strings"
)

func buildMCPPromptContext(serverNames []string) string {
	if len(serverNames) == 0 {
		return ""
	}

	return fmt.Sprintf(`# MCP Servers

Connected MCP servers: %s

MCP tools are prefixed as mcp__<server>__<tool>.`,
		strings.Join(serverNames, ", "),
	)
}
