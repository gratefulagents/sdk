package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// Tools returns the skill search, install, and list-installed tools.
func Tools(registry *Registry, installer *Installer, workDir string) []agentsdk.Tool {
	return []agentsdk.Tool{
		&SearchTool{Registry: registry},
		&InstallTool{Installer: installer, WorkDir: workDir},
		&ListInstalledTool{Installer: installer, WorkDir: workDir},
	}
}

// SearchTool searches the skill catalog by query, category, or tag.
type SearchTool struct {
	Registry *Registry
}

func (t *SearchTool) Name() string { return "skill_search" }
func (t *SearchTool) Description() string {
	return "Search the skill catalog for MCP tools that can be installed. Use this to discover available integrations (search, browser, GitHub, Slack, etc)."
}
func (t *SearchTool) IsReadOnly() bool { return true }
func (t *SearchTool) IsEnabled(_ *agentsdk.RunContext) bool {
	return true
}
func (t *SearchTool) NeedsApproval() bool { return false }
func (t *SearchTool) TimeoutSeconds() int { return 0 }
func (t *SearchTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query (matches name, description, tags)"},
			"category": {"type": "string", "description": "Filter by category (e.g. search, browser, developer-tools, communication, data)"}
		}
	}`)
}

func (t *SearchTool) Execute(_ context.Context, input json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	if t.Registry == nil {
		return agentsdk.ToolResult{Content: "skill_search requires a configured registry", IsError: true}, nil
	}
	var in struct {
		Query    string `json:"query"`
		Category string `json:"category"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &in); err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
		}
	}

	var skills []SkillEntry
	if in.Query != "" {
		skills = t.Registry.Search(in.Query)
	} else {
		var opts []FilterOption
		if in.Category != "" {
			opts = append(opts, WithCategory(in.Category))
		}
		skills = t.Registry.List(opts...)
	}

	if len(skills) == 0 {
		return agentsdk.ToolResult{Content: "No skills found matching your criteria."}, nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d skill(s):\n\n", len(skills)))
	for _, skill := range skills {
		verified := ""
		if skill.Verified {
			verified = " ✓"
		}
		sb.WriteString(fmt.Sprintf("• **%s** (v%s, %s%s)\n  %s\n  Tags: %s\n\n",
			skill.Name, skill.Version, skill.Category, verified, skill.Description, strings.Join(skill.Tags, ", ")))
	}
	return agentsdk.ToolResult{Content: sb.String()}, nil
}

// InstallTool installs a skill from the catalog into the workspace's .mcp.json.
type InstallTool struct {
	Installer *Installer
	WorkDir   string
}

func (t *InstallTool) Name() string { return "skill_install" }
func (t *InstallTool) Description() string {
	return "Install a skill from the catalog into the workspace. This writes the MCP server config to .mcp.json so it becomes available as a tool."
}
func (t *InstallTool) IsReadOnly() bool { return false }
func (t *InstallTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}
func (t *InstallTool) NeedsApproval() bool { return false }
func (t *InstallTool) TimeoutSeconds() int { return 0 }
func (t *InstallTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Skill name to install (e.g. search-duckduckgo, browser-playwright)"}
		},
		"required": ["name"]
	}`)
}

func (t *InstallTool) Execute(_ context.Context, input json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	if t.Installer == nil {
		return agentsdk.ToolResult{Content: "skill_install requires a configured installer", IsError: true}, nil
	}
	var in struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}

	dir := workDir
	if dir == "" {
		dir = t.WorkDir
	}

	if err := t.Installer.Install(dir, in.Name); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to install skill %q: %v", in.Name, err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Skill %q installed successfully. Its MCP server config has been added to .mcp.json.", in.Name)}, nil
}

// ListInstalledTool lists skills currently installed in the workspace.
type ListInstalledTool struct {
	Installer *Installer
	WorkDir   string
}

func (t *ListInstalledTool) Name() string { return "skill_list_installed" }
func (t *ListInstalledTool) Description() string {
	return "List skills currently installed in the workspace's .mcp.json."
}
func (t *ListInstalledTool) IsReadOnly() bool { return true }
func (t *ListInstalledTool) IsEnabled(_ *agentsdk.RunContext) bool {
	return true
}
func (t *ListInstalledTool) NeedsApproval() bool { return false }
func (t *ListInstalledTool) TimeoutSeconds() int { return 0 }
func (t *ListInstalledTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *ListInstalledTool) Execute(_ context.Context, _ json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	if t.Installer == nil {
		return agentsdk.ToolResult{Content: "skill_list_installed requires a configured installer", IsError: true}, nil
	}
	dir := workDir
	if dir == "" {
		dir = t.WorkDir
	}

	names, err := t.Installer.ListInstalled(dir)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to list installed skills: %v", err), IsError: true}, nil
	}

	if len(names) == 0 {
		return agentsdk.ToolResult{Content: "No skills currently installed in .mcp.json."}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Installed skills: %s", strings.Join(names, ", "))}, nil
}
