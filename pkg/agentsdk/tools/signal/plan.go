package signal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// ArtifactStore is the persistence surface needed by save_plan/get_plan.
type ArtifactStore interface {
	UpsertArtifact(ctx context.Context, sessionID uuid.UUID, kind, content, s3URL, contentHash string, metadata json.RawMessage) (any, error)
	GetArtifact(ctx context.Context, sessionID uuid.UUID, kind string) (*Artifact, error)
}

// Artifact is the SDK-native shape used by get_plan.
type Artifact struct {
	Content string
}

// ErrArtifactNotFound lets adapters map their backend's not-found sentinel.
var ErrArtifactNotFound = errors.New("artifact not found")

// PlanTools returns save_plan and get_plan tools backed by an ArtifactStore.
func PlanTools(store ArtifactStore, sessionID uuid.UUID) []agentsdk.Tool {
	return []agentsdk.Tool{
		&SavePlanTool{Store: store, SessionID: sessionID},
		&GetPlanTool{Store: store, SessionID: sessionID},
	}
}

// SavePlanTool persists an implementation plan.
type SavePlanTool struct {
	Store     ArtifactStore
	SessionID uuid.UUID
}

func (t *SavePlanTool) Name() string { return "save_plan" }

func (t *SavePlanTool) Description() string {
	return `Save the implementation plan to a durable plan artifact.

This plan survives phase and step changes. Always call save_plan at the end of
planning instead of only putting the plan in conversation text.`
}

func (t *SavePlanTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"plan": {"type": "string", "description": "The full plan content in markdown format"},
			"summary": {"type": "string", "description": "A 1-2 sentence summary of the plan"}
		},
		"required": ["plan"]
	}`)
}

func (t *SavePlanTool) IsReadOnly() bool { return false }

func (t *SavePlanTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *SavePlanTool) NeedsApproval() bool { return false }

func (t *SavePlanTool) TimeoutSeconds() int { return 0 }

type savePlanInput struct {
	Plan    string `json:"plan"`
	Summary string `json:"summary"`
}

func (t *SavePlanTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in savePlanInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if in.Plan == "" {
		return agentsdk.ToolResult{Content: "plan content is required", IsError: true}, nil
	}
	if t.Store == nil {
		return agentsdk.ToolResult{Content: "save_plan requires a configured artifact store", IsError: true}, nil
	}

	summary := in.Summary
	if summary == "" && len(in.Plan) > 200 {
		summary = in.Plan[:200] + "..."
	} else if summary == "" {
		summary = in.Plan
	}
	meta, _ := json.Marshal(map[string]string{
		"summary":    summary,
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if _, err := t.Store.UpsertArtifact(ctx, t.SessionID, "plan", in.Plan, "", "", meta); err != nil {
		log.Printf("WARN: failed to persist plan: %v", err)
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to persist plan: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Plan saved (%d bytes) to artifact store", len(in.Plan))}, nil
}

// GetPlanTool reads a persisted implementation plan.
type GetPlanTool struct {
	Store     ArtifactStore
	SessionID uuid.UUID
}

func (t *GetPlanTool) Name() string { return "get_plan" }

func (t *GetPlanTool) Description() string {
	return "Read the current implementation plan from the durable plan artifact."
}

func (t *GetPlanTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}

func (t *GetPlanTool) IsReadOnly() bool { return true }

func (t *GetPlanTool) IsEnabled(_ *agentsdk.RunContext) bool { return true }

func (t *GetPlanTool) NeedsApproval() bool { return false }

func (t *GetPlanTool) TimeoutSeconds() int { return 0 }

func (t *GetPlanTool) Execute(ctx context.Context, _ json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	if t.Store == nil {
		return agentsdk.ToolResult{Content: "get_plan requires a configured artifact store", IsError: true}, nil
	}
	art, err := t.Store.GetArtifact(ctx, t.SessionID, "plan")
	if err != nil {
		if errors.Is(err, ErrArtifactNotFound) {
			return agentsdk.ToolResult{Content: "No plan found. Use save_plan to create one first."}, nil
		}
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to read plan: %v", err), IsError: true}, nil
	}
	if art == nil || art.Content == "" {
		return agentsdk.ToolResult{Content: "No plan found. Use save_plan to create one first."}, nil
	}
	return agentsdk.ToolResult{Content: art.Content}, nil
}
