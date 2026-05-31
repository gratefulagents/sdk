package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// BreakGlassRequest is the SDK-native shape for requesting temporary MCP access.
type BreakGlassRequest struct {
	Server string
	Tool   string
	Reason string
}

// BreakGlassPolicy describes host-neutral MCP break-glass display rules.
type BreakGlassPolicy struct {
	RequireAuditReason bool
}

func BreakGlassActions() json.RawMessage {
	return agentsdk.MarshalQuickActions(
		agentsdk.QuickAction{ID: "approve", Label: "Approve", Style: "primary"},
		agentsdk.QuickAction{ID: "reject", Label: "Reject", Style: "destructive"},
	)
}

func BuildBreakGlassQuestion(request BreakGlassRequest, cfg BreakGlassPolicy) string {
	target := FormatBreakGlassTarget(request.Server, request.Tool)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Approve temporary MCP access to %s?", target))
	if strings.TrimSpace(request.Reason) != "" {
		b.WriteString("\n\nReason: " + strings.TrimSpace(request.Reason))
	}
	if cfg.RequireAuditReason {
		b.WriteString("\n\nThis policy requires an audit reason.")
	}
	return b.String()
}

func FormatBreakGlassTarget(server, tool string) string {
	server = strings.TrimSpace(server)
	tool = strings.TrimSpace(tool)
	if tool != "" {
		return fmt.Sprintf("server %q tool %q", server, tool)
	}
	return fmt.Sprintf("server %q", server)
}

func SummarizeBreakGlassTarget(verb, server, tool string) string {
	return fmt.Sprintf("%s MCP access for %s", strings.TrimSpace(verb), FormatBreakGlassTarget(server, tool))
}

func BlockedToolMessage(server, tool string, breakGlassEnabled bool) string {
	msg := fmt.Sprintf("MCP tool %q on server %q is blocked by policy.", strings.TrimSpace(tool), strings.TrimSpace(server))
	if breakGlassEnabled {
		msg += " Use RequestMCPBreakGlass with a reason if this access is needed."
	}
	return msg
}

func BlockedServerMessage(server string, breakGlassEnabled bool) string {
	msg := fmt.Sprintf("MCP server %q is blocked by policy.", strings.TrimSpace(server))
	if breakGlassEnabled {
		msg += " Use RequestMCPBreakGlass with a reason if this access is needed."
	}
	return msg
}
