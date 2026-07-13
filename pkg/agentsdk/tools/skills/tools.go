package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
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
		sb.WriteString(fmt.Sprintf("• **%s** (v%s, %s%s)\n  %s\n  Tags: %s\n",
			skill.Name, skill.Version, skill.Category, verified, skill.Description, strings.Join(skill.Tags, ", ")))
		if len(skill.RequiresEnvVars) > 0 {
			sb.WriteString(fmt.Sprintf("  Requires env: %s\n", describeEnvVars(skill.RequiresEnvVars)))
		}
		sb.WriteString("\n")
	}
	return agentsdk.ToolResult{Content: sb.String()}, nil
}

// describeEnvVars annotates each required env var with whether it is
// currently present in the agent's environment, so callers can tell up front
// whether an installed skill will be able to authenticate.
func describeEnvVars(names []string) string {
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if v, ok := os.LookupEnv(name); ok && strings.TrimSpace(v) != "" {
			parts = append(parts, name+" (set)")
		} else {
			parts = append(parts, name+" (NOT set)")
		}
	}
	return strings.Join(parts, ", ")
}

// InstallTool installs a skill from the catalog into the workspace's .mcp.json.
type InstallTool struct {
	Installer *Installer
	WorkDir   string
}

func (t *InstallTool) Name() string { return "skill_install" }
func (t *InstallTool) Description() string {
	return "Install a skill from the catalog into the workspace. This writes the MCP server config to .mcp.json. MCP configs are loaded at session start, so the new server's tools become available after the agent restarts (they are NOT available in the current session)."
}
func (t *InstallTool) IsReadOnly() bool { return false }
func (t *InstallTool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return agentsdk.MutatingToolEnabled(ctx, t.Name())
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

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Skill %q installed: its MCP server config was added to .mcp.json.", in.Name))
	sb.WriteString(" MCP configs are loaded at session start, so its tools become available after the agent restarts — they are NOT available in this session.")
	if skill, ok := t.Installer.Skill(in.Name); ok {
		if missing := missingEnvVars(skill.RequiresEnvVars); len(missing) > 0 {
			sb.WriteString(fmt.Sprintf("\n\nWARNING: required environment variable(s) not set: %s. The server will likely fail to authenticate until they are provided (e.g. via the host's secret mechanism or the entry's env map in .mcp.json; they are already listed in the entry's allowEnv).",
				strings.Join(missing, ", ")))
		}
	}
	return agentsdk.ToolResult{Content: sb.String()}, nil
}

// missingEnvVars returns the subset of names absent or empty in the agent's
// environment.
func missingEnvVars(names []string) []string {
	var missing []string
	for _, name := range names {
		if v, ok := os.LookupEnv(name); !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, name)
		}
	}
	return missing
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

	sort.Strings(names)
	var skills, others []string
	for _, name := range names {
		if _, ok := t.Installer.Skill(name); ok {
			skills = append(skills, name)
		} else {
			others = append(others, name)
		}
	}

	var lines []string
	if len(skills) > 0 {
		lines = append(lines, fmt.Sprintf("Installed skills: %s", strings.Join(skills, ", ")))
	} else {
		lines = append(lines, "No skills from the catalog are installed in .mcp.json.")
	}
	if len(others) > 0 {
		lines = append(lines, fmt.Sprintf("Other MCP servers in .mcp.json (not from the skill catalog): %s", strings.Join(others, ", ")))
	}
	return agentsdk.ToolResult{Content: strings.Join(lines, "\n")}, nil
}
