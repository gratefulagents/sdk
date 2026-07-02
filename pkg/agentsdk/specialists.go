package agentsdk

import (
	"fmt"
	"sort"
	"strings"

	sdkmode "github.com/gratefulagents/sdk/pkg/agentsdk/mode"
)

// RoleSpec is the SDK-native shape for a specialist role.
type RoleSpec struct {
	Name           string
	Description    string
	Instructions   string
	ToolAccess     string
	ModelOverride  string
	FallbackModels []string
}

type RoleCatalog []RoleSpec

// SpecialistBuildOptions controls conversion from role specs to runnable
// sub-agent tools.
type SpecialistBuildOptions struct {
	Runner            *Runner
	Tools             []Tool
	BaseModel         string
	FallbackModels    []string
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

// ModeRoutingFromTemplateSpec converts SDK mode routing config into runner
// routing config.
func ModeRoutingFromTemplateSpec(spec *sdkmode.TemplateSpec) *ModeModelRouting {
	if spec == nil || spec.ModelRouting == nil {
		return nil
	}
	out := &ModeModelRouting{
		DefaultModel:   spec.ModelRouting.DefaultModel,
		FallbackModels: append([]string(nil), spec.ModelRouting.FallbackModels...),
		ReasoningLevel: spec.ModelRouting.ReasoningLevel,
		TextVerbosity:  spec.ModelRouting.TextVerbosity,
	}
	if len(spec.ModelRouting.RoleOverrides) > 0 {
		out.RoleOverrides = map[string]ModeRoleModelRouting{}
		for name, override := range spec.ModelRouting.RoleOverrides {
			out.RoleOverrides[name] = ModeRoleModelRouting{
				Model:          override.Model,
				FallbackModels: append([]string(nil), override.FallbackModels...),
				ReasoningLevel: override.ReasoningLevel,
				TextVerbosity:  override.TextVerbosity,
			}
		}
	}
	return out
}

// BuildDelegationGuide generates structured delegation instructions describing
// the specialist sub-agents reachable through the subagent tool and lateral
// handoffs available to an agent.
func BuildDelegationGuide(a *Agent, specialists map[string]*Agent) string {
	var subAgents, handoffs []string
	names := make([]string, 0, len(specialists))
	for name := range specialists {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		desc := specialists[name].HandoffDescription
		if desc == "" {
			desc = "Specialist sub-agent"
		}
		subAgents = append(subAgents, fmt.Sprintf("- %s: %s", name, desc))
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
		b.WriteString("Available specialist sub-agents (delegate via the subagent tool, agent_name=<name>):\n")
		b.WriteString(strings.Join(subAgents, "\n"))
		b.WriteString("\n\n")
		b.WriteString(`CRITICAL: When delegating, the "message" is the ONLY context the sub-agent receives.
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

SYNC VS BACKGROUND: mode="sync" (default) returns the result in the same subagent call — use it when you need the result before continuing.
mode="background" returns task ids immediately so you can keep working; each result is delivered to you automatically as soon as its task finishes.
Only use background mode when you have genuinely independent work to do meanwhile — do not spawn and then idle-poll.

FILE ISOLATION: When running parallel sub-agents, assign each a disjoint set of files.
Never let two concurrent sub-agents edit the same file. If tasks must touch the same file, run them sequentially.
Include "Files you own: [...]" in each message so the sub-agent stays in its lane.

DAG WORKFLOWS: If tasks depend on each other, describe the whole graph in one subagent call using tasks=[{key, message, depends_on:[keys]}, ...] instead of manual sequencing.
depends_on entries reference other task keys in the same call (or existing task ids); dependency outputs are passed to downstream tasks as context by default.
mode="sync" on a tasks array waits for the whole graph and returns all results; mode="background" returns the task ids immediately.

EFFORT SCALING: Match delegation to task complexity instead of defaulting to maximum fan-out.
- Simple lookups or single-file questions: answer directly with your own tools, or at most 1 read-only sub-agent.
- Comparisons or multi-area investigations: 2-4 parallel read-only sub-agents, each owning one distinct question.
- Large builds or genuinely decomposable implementation work: more sub-agents, but only with disjoint file ownership.
State an explicit effort budget in each task packet (e.g. "aim for under 10 tool calls") so sub-agents do not over-explore.

ARTIFACTS: For large deliverables (reports, generated code listings, long logs), instruct the sub-agent to write the full output to a file under the working directory and return a concise summary plus the file path.
Read the file yourself only when needed. Passing lightweight references instead of full content avoids losing information when many results flow through you.

STATUS AND RESULTS: Background task progress streams through the runtime; you do not need to poll to check whether tasks are still running.
Completed task results are delivered to you automatically as soon as each task finishes; act on early results while slower tasks keep running.
Use subagent_status (detail="activity") for evidence on a specific task before steering; detail="graph" shows the dependency DAG.
The SDK will not let you final-answer while background tasks are still active; keep supervising until results arrive.

STEERING: If a running task needs an update, correction, or narrower constraint, subagent_control action="message" queues a parent message for its next model turn; action="cancel" stops it.
Keep steering messages short and specific.`)
		b.WriteString("\n\n")
	}

	if len(handoffs) > 0 {
		b.WriteString("Handoffs (transfer full control):\n")
		b.WriteString(strings.Join(handoffs, "\n"))
		b.WriteString("\n\n")
	}

	b.WriteString("Signal tools: finish\n")
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
			FallbackModels:     append([]string(nil), role.FallbackModels...),
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
		fallbackModels := append([]string(nil), opts.FallbackModels...)
		settings := opts.BaseModelSettings
		if strings.TrimSpace(role.ModelOverride) != "" {
			model = strings.TrimSpace(role.ModelOverride)
		}
		if len(role.FallbackModels) > 0 {
			fallbackModels = append([]string(nil), role.FallbackModels...)
		}
		if routing != nil {
			resolved := ResolveRoleModeRouting(model, opts.Provider, name, routing)
			if strings.TrimSpace(resolved.Model) != "" {
				model = resolved.Model
			}
			if len(resolved.FallbackModels) > 0 {
				fallbackModels = append([]string(nil), resolved.FallbackModels...)
			}
			settings = settings.Merge(resolved.ModelSettings)
		}

		filteredTools := FilterToolsByAccess(opts.Tools, role.ToolAccess)
		filteredTools = StripSignalTools(filteredTools)
		agent := &Agent{
			Name:               name,
			Instructions:       role.Instructions,
			Model:              model,
			FallbackModels:     fallbackModels,
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
