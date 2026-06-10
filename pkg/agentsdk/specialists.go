package agentsdk

import (
	"fmt"
	"strings"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

// RoleSpec is the SDK-native shape for a specialist role.
type RoleSpec struct {
	Name          string
	Description   string
	Instructions  string
	ToolAccess    string
	ModelOverride string
}

type RoleCatalog []RoleSpec

// SpecialistBuildOptions controls conversion from role specs to runnable
// sub-agent tools.
type SpecialistBuildOptions struct {
	Runner            *Runner
	Tools             []Tool
	BaseModel         string
	Provider          string
	BaseModelSettings ModelSettings
	ModeSnapshot      *sdkmode.TemplateSpec

	// OutputExtractor, when set, post-processes each specialist sub-agent's
	// RunResult before its text is returned to the parent agent.
	OutputExtractor func(*RunResult) string
}

// SpecialistBuildResult holds both sub-agent tools and the raw agents.
type SpecialistBuildResult struct {
	Tools  []Tool
	Agents map[string]*Agent
}

// FilterToolsByAccess returns the subset of tools matching the given access level.
// Tools that implement ToolAccessAdapter may substitute a safer implementation
// for read-only access before filtering.
func FilterToolsByAccess(allTools []Tool, accessLevel string) []Tool {
	switch accessLevel {
	case "read-only", "analysis":
		var filtered []Tool
		for _, t := range allTools {
			if adapter, ok := t.(ToolAccessAdapter); ok {
				if adapted := adapter.ToolForAccess(ToolAccessLevelReadOnly); adapted != nil {
					t = adapted
				}
			}
			if t.IsReadOnly() {
				filtered = append(filtered, t)
			}
		}
		return filtered
	default:
		result := make([]Tool, len(allTools))
		copy(result, allTools)
		return result
	}
}

// DefaultSignalToolNames returns the orchestrator-only tool names stripped from specialists.
func DefaultSignalToolNames() map[string]bool {
	return map[string]bool{
		"finish":          true,
		"set_phase":       true,
		"present_plan":    true,
		"AskUserQuestion": true,
	}
}

// StripSignalTools removes orchestrator-only signal tools from a tool list.
func StripSignalTools(tools []Tool) []Tool {
	return StripNamedSignalTools(tools, DefaultSignalToolNames())
}

func StripNamedSignalTools(tools []Tool, signalToolNames map[string]bool) []Tool {
	filtered := make([]Tool, 0, len(tools))
	for _, t := range tools {
		if !signalToolNames[t.Name()] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// ToolAccessLevelFromRole normalizes role/catalog tool access strings.
func ToolAccessLevelFromRole(value string) ToolAccessLevel {
	if strings.TrimSpace(value) == "" {
		return ToolAccessLevelFull
	}
	return NormalizeToolAccessLevel(ToolAccessLevel(value))
}

// FilterRoleCatalogForMode narrows a catalog to the capabilities declared by a
// mode template. If a mode names capabilities but none match local roles, the
// original catalog is returned so hosts do not silently lose all specialists.
func FilterRoleCatalogForMode(catalog RoleCatalog, spec *sdkmode.TemplateSpec) RoleCatalog {
	if spec == nil || len(spec.Capabilities) == 0 {
		return catalog
	}
	allowed := map[string]bool{}
	for _, name := range spec.Capabilities {
		if trimmed := strings.TrimSpace(name); trimmed != "" {
			allowed[trimmed] = true
		}
	}
	if len(allowed) == 0 {
		return catalog
	}
	filtered := make(RoleCatalog, 0, len(catalog))
	for _, role := range catalog {
		if allowed[role.Name] {
			filtered = append(filtered, role)
		}
	}
	if len(filtered) == 0 {
		return catalog
	}
	return filtered
}

// ModeRoutingFromTemplateSpec converts SDK mode routing config into runner
// routing config.
func ModeRoutingFromTemplateSpec(spec *sdkmode.TemplateSpec) *ModeModelRouting {
	if spec == nil || spec.ModelRouting == nil {
		return nil
	}
	out := &ModeModelRouting{
		DefaultModel:   spec.ModelRouting.DefaultModel,
		ReasoningLevel: spec.ModelRouting.ReasoningLevel,
		TextVerbosity:  spec.ModelRouting.TextVerbosity,
	}
	if len(spec.ModelRouting.RoleOverrides) > 0 {
		out.RoleOverrides = map[string]ModeRoleModelRouting{}
		for name, override := range spec.ModelRouting.RoleOverrides {
			out.RoleOverrides[name] = ModeRoleModelRouting{
				Model:          override.Model,
				ReasoningLevel: override.ReasoningLevel,
				TextVerbosity:  override.TextVerbosity,
			}
		}
	}
	return out
}

// BuildDelegationGuide generates structured delegation instructions describing
// specialist sub-agents and lateral handoffs available to an agent.
func BuildDelegationGuide(a *Agent) string {
	var subAgents, handoffs []string
	for _, t := range a.Tools {
		name := t.Name()
		if len(name) > 6 && name[:6] == "agent_" {
			desc := t.Description()
			if desc == "" {
				desc = "Specialist sub-agent"
			}
			subAgents = append(subAgents, fmt.Sprintf("- %s: %s", name, desc))
		}
	}
	for _, h := range a.Handoffs {
		desc := h.Description
		if desc == "" {
			desc = fmt.Sprintf("Transfer to %s", h.Agent.Name)
		}
		handoffs = append(handoffs, fmt.Sprintf("- %s: %s", h.ToolName, desc))
	}

	if len(subAgents) == 0 && len(handoffs) == 0 {
		return ""
	}

	var b strings.Builder
	if len(subAgents) > 0 {
		b.WriteString("Available sub-agents (call as tools for bounded tasks):\n")
		b.WriteString(strings.Join(subAgents, "\n"))
		b.WriteString("\n\n")
		b.WriteString(`CRITICAL: When delegating to a sub-agent, your "message" is the ONLY context it receives.
Give each sub-agent a compact, self-contained task packet: enough context to execute without guessing, but no extra history.
Include only the pieces that materially affect this task:
- the exact task or deliverable for this sub-agent
- the goal or outcome this task must support
- relevant repo context: file paths, symbols, interfaces, observed behavior, or failing commands
- constraints, acceptance criteria, and non-goals that shape the task
- prior decisions, findings, or draft work this task depends on
- the expected output format and any file ownership boundaries

Distill the context; do NOT paste the user's original request verbatim or dump irrelevant history.
Do NOT send the same large background block to every task if only one sub-agent needs it.
Planning/review/design tasks usually need the broader objective plus the current findings or draft they are building on.
Execution/test/fix tasks usually need the concrete slice they own plus adjacent contracts they must preserve.

FILE ISOLATION: When spawning parallel sub-agents, assign each a disjoint set of files.
Never let two concurrent sub-agents edit the same file. If tasks must touch the same file, run them sequentially.
Include "Files you own: [...]" in each message so the sub-agent stays in its lane.

DAG WORKFLOWS: If a sub-agent needs another sub-agent's result before starting, use dependencies instead of manual polling.
Use run_subagent_task for normal delegate-and-return work.
Use spawn_subagent_graph with return_when="all_complete" for multi-step DAG joins, or depends_on in spawn_subagent_task for managed downstream work.
By default, dependency outputs are passed to downstream tasks as context once all dependencies complete.

STATUS REPORTING: Managed sub-agent progress streams through the runtime while the SDK keeps the parent run open. You do not need to call a polling/wait tool just to check whether tasks are still running.
Completed task results are delivered to you automatically as soon as each task finishes; act on early results while slower tasks keep running.
Use get_subagent_activity for deeper detail on a specific task when you need evidence before steering.
The SDK will not let the parent final-answer while managed sub-agent tasks are still active; keep supervising until results arrive.

STEERING: If a running sub-agent needs an update, correction, or narrower constraint, send_message_to_subagent_task can queue a parent message for its next model turn.
Keep steering messages short and specific.`)
		b.WriteString("\n\n")
	}

	if len(handoffs) > 0 {
		b.WriteString("Handoffs (transfer full control):\n")
		b.WriteString(strings.Join(handoffs, "\n"))
		b.WriteString("\n\n")
	}

	b.WriteString("Signal tools: finish, set_phase\n")
	return b.String()
}

// BuildSpecialistsFromCatalog builds Agent values from SDK-native role specs.
func BuildSpecialistsFromCatalog(catalog RoleCatalog) []*Agent {
	agents := make([]*Agent, 0, len(catalog))
	for _, role := range catalog {
		if strings.TrimSpace(role.Name) == "" {
			continue
		}
		agents = append(agents, &Agent{
			Name:               role.Name,
			Instructions:       role.Instructions,
			Model:              role.ModelOverride,
			HandoffDescription: role.Description,
		})
	}
	return agents
}

// BuildSpecialistToolsFromCatalog builds sub-agent tools from SDK-native role
// specs, applying tool access, signal stripping, and mode model routing.
func BuildSpecialistToolsFromCatalog(catalog RoleCatalog, opts SpecialistBuildOptions) SpecialistBuildResult {
	result := SpecialistBuildResult{
		Agents: map[string]*Agent{},
	}
	if opts.Runner == nil {
		return result
	}
	routing := ModeRoutingFromTemplateSpec(opts.ModeSnapshot)
	seen := map[string]bool{}
	for _, role := range catalog {
		name := strings.TrimSpace(role.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		model := opts.BaseModel
		settings := opts.BaseModelSettings
		if strings.TrimSpace(role.ModelOverride) != "" {
			model = strings.TrimSpace(role.ModelOverride)
		}
		if routing != nil {
			resolved := ResolveRoleModeRouting(model, opts.Provider, name, routing)
			if strings.TrimSpace(resolved.Model) != "" {
				model = resolved.Model
			}
			settings = settings.Merge(resolved.ModelSettings)
		}

		filteredTools := FilterToolsByAccess(opts.Tools, role.ToolAccess)
		filteredTools = StripSignalTools(filteredTools)
		agent := &Agent{
			Name:               name,
			Instructions:       role.Instructions,
			Model:              model,
			ModelSettings:      settings,
			Tools:              filteredTools,
			HandoffDescription: role.Description,
		}
		result.Agents[name] = agent
		var asToolOpts []AsToolOption
		if opts.OutputExtractor != nil {
			asToolOpts = append(asToolOpts, WithAsToolOutputExtractor(opts.OutputExtractor))
		}
		result.Tools = append(result.Tools, agent.AsTool(opts.Runner, asToolOpts...))
	}
	return result
}
