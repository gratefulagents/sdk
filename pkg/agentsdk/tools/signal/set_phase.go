package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// PhaseSink is notified when an agent declares a new workflow phase.
type PhaseSink interface {
	SetPhase(ctx context.Context, phase string) error
}

// PhaseTransitionSink can handle a phase transition and provide a host-specific
// tool response while the SDK owns the tool schema and validation.
type PhaseTransitionSink interface {
	SetPhaseTransition(ctx context.Context, request PhaseChangeRequest) (PhaseChangeResult, error)
}

type PhaseChangeRequest struct {
	Phase        string
	CurrentPhase string
	Option       PhaseOption
	Known        bool
	WorkDir      string
}

type PhaseChangeResult struct {
	Content     string
	IsError     bool
	ShouldPause bool
}

// PhaseOption describes one valid phase for SetPhaseTool.
type PhaseOption struct {
	ID               string
	ReadOnly         bool
	RequiresApproval bool
	Description      string
}

// SetPhaseTool lets an agent declare the active workflow phase.
//
// Hosts that change tool access or other runtime settings by phase should set
// PauseOnChange and resume the run with a rebuilt RunConfig after the tool
// result is appended to history.
type SetPhaseTool struct {
	Phases        []PhaseOption
	CurrentPhase  string
	Sink          PhaseSink
	PauseOnChange bool
}

func (t *SetPhaseTool) Name() string { return "set_phase" }

func (t *SetPhaseTool) Description() string {
	base := "Declare the current phase of work. Use this when moving through the active mode's workflow."
	if len(t.Phases) == 0 {
		return base
	}
	var parts []string
	for _, phase := range t.Phases {
		if strings.TrimSpace(phase.ID) == "" {
			continue
		}
		label := phase.ID
		var flags []string
		if phase.ReadOnly {
			flags = append(flags, "read-only")
		}
		if phase.RequiresApproval {
			flags = append(flags, "approval")
		}
		if len(flags) > 0 {
			label += " (" + strings.Join(flags, ", ") + ")"
		}
		parts = append(parts, label)
	}
	if len(parts) == 0 {
		return base
	}
	return base + " Phases for this mode: " + strings.Join(parts, ", ") + "."
}

func (t *SetPhaseTool) InputSchema() json.RawMessage {
	ids := t.phaseIDs()
	if len(ids) > 0 {
		enumBytes, _ := json.Marshal(ids)
		return json.RawMessage(fmt.Sprintf(`{
			"type": "object",
			"properties": {
				"phase": {
					"type": "string",
					"description": "Current phase label",
					"enum": %s
				}
			},
			"required": ["phase"]
		}`, string(enumBytes)))
	}
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"phase": {
				"type": "string",
				"description": "Current phase label"
			}
		},
		"required": ["phase"]
	}`)
}

func (t *SetPhaseTool) IsReadOnly() bool { return false }

func (t *SetPhaseTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *SetPhaseTool) NeedsApproval() bool { return false }

func (t *SetPhaseTool) TimeoutSeconds() int { return 0 }

func (t *SetPhaseTool) Execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	return t.execute(ctx, raw, workDir)
}

func (t *SetPhaseTool) execute(ctx context.Context, raw json.RawMessage, workDir string) (agentsdk.ToolResult, error) {
	var in struct {
		Phase string `json:"phase"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	phase := strings.TrimSpace(in.Phase)
	transitionSink, hasTransitionSink := t.Sink.(PhaseTransitionSink)
	if phase == "" {
		if !hasTransitionSink {
			return agentsdk.ToolResult{Content: "phase is required", IsError: true}, nil
		}
		result, err := transitionSink.SetPhaseTransition(ctx, PhaseChangeRequest{WorkDir: workDir})
		return agentsdk.ToolResult{Content: result.Content, IsError: result.IsError, ShouldPause: result.ShouldPause}, err
	}
	option, known := t.findPhase(phase)
	if len(t.phaseIDs()) > 0 && !known {
		return agentsdk.ToolResult{Content: fmt.Sprintf("phase %q is not valid for this mode", phase), IsError: true}, nil
	}
	if hasTransitionSink {
		result, err := transitionSink.SetPhaseTransition(ctx, PhaseChangeRequest{
			Phase:        phase,
			CurrentPhase: strings.TrimSpace(t.CurrentPhase),
			Option:       option,
			Known:        known,
			WorkDir:      workDir,
		})
		return agentsdk.ToolResult{Content: result.Content, IsError: result.IsError, ShouldPause: result.ShouldPause}, err
	}
	if t.Sink != nil {
		if err := t.Sink.SetPhase(ctx, phase); err != nil {
			return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to set phase: %v", err), IsError: true}, nil
		}
	}

	status := "changed"
	if strings.TrimSpace(t.CurrentPhase) == phase {
		status = "unchanged"
	}
	access := "full"
	if known && option.ReadOnly {
		access = "read-only"
	}
	response := map[string]any{
		"phase":       phase,
		"status":      status,
		"tool_access": access,
		"message":     "Phase set to: " + phase,
	}
	if known && option.RequiresApproval {
		response["requires_approval"] = true
	}
	data, _ := json.Marshal(response)
	return agentsdk.ToolResult{
		Content:     string(data),
		ShouldPause: t.PauseOnChange && status == "changed",
	}, nil
}

func (t *SetPhaseTool) phaseIDs() []string {
	ids := make([]string, 0, len(t.Phases))
	for _, phase := range t.Phases {
		if strings.TrimSpace(phase.ID) != "" {
			ids = append(ids, phase.ID)
		}
	}
	return ids
}

func (t *SetPhaseTool) findPhase(id string) (PhaseOption, bool) {
	for _, phase := range t.Phases {
		if phase.ID == id {
			return phase, true
		}
	}
	return PhaseOption{}, false
}
