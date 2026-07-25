package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// DurableCheckpointSchemaVersion is the current stable JSON checkpoint schema.
const DurableCheckpointSchemaVersion = 1

// DurableBoundary identifies a point after which a run may be continued on a
// different process without repeating the preceding SDK operation.
type DurableBoundary string

const (
	DurableBoundaryRunStarted       DurableBoundary = "run_started"
	DurableBoundaryModelPrepared    DurableBoundary = "model_prepared"
	DurableBoundaryModelCompleted   DurableBoundary = "model_completed"
	DurableBoundaryToolPrepared     DurableBoundary = "tool_prepared"
	DurableBoundaryToolCompleted    DurableBoundary = "tool_completed"
	DurableBoundaryApprovalPending  DurableBoundary = "approval_pending"
	DurableBoundaryHandoffCompleted DurableBoundary = "handoff_completed"
	DurableBoundaryChildChanged     DurableBoundary = "child_changed"
	DurableBoundaryPaused           DurableBoundary = "paused"
	DurableBoundaryRunCancelled     DurableBoundary = "run_cancelled"
	DurableBoundaryRunCompleted     DurableBoundary = "run_completed"
)

// DurableCheckpoint is the versioned, function-free continuation emitted by
// Runner at every external execution boundary. History is deliberately made
// from LLMRunItemSnapshot rather than RunItem so agent pointers and callbacks
// can never leak into persisted JSON.
type DurableCheckpoint struct {
	SchemaVersion int                          `json:"schema_version"`
	RunID         string                       `json:"run_id"`
	AttemptID     string                       `json:"attempt_id"`
	StepID        string                       `json:"step_id"`
	Sequence      uint64                       `json:"sequence"`
	Boundary      DurableBoundary              `json:"boundary"`
	AgentName     string                       `json:"agent_name"`
	History       []LLMRunItemSnapshot         `json:"history"`
	Interruptions []*Interruption              `json:"interruptions,omitempty"`
	Usage         Usage                        `json:"usage"`
	Children      *SubAgentSchedulerCheckpoint `json:"children,omitempty"`
	CreatedAt     time.Time                    `json:"created_at"`
}

// DurableCheckpointHook persists a checkpoint. Returning an error fails the
// run closed: execution never crosses a boundary that could not be recorded.
type DurableCheckpointHook func(context.Context, DurableCheckpoint) error

// DurableRunConfig enables runner-owned checkpoints. It is optional; a nil
// config preserves the original entirely in-process Runner behavior.
type DurableRunConfig struct {
	RunID      string
	AttemptID  string
	Resume     *DurableCheckpoint
	Checkpoint DurableCheckpointHook
	Children   func() SubAgentSchedulerCheckpoint
}

func durableID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is exceptional; timestamp still avoids silently
		// emitting an empty identity and lets the store reject a collision.
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}

func (c *DurableRunConfig) normalize() {
	if c.RunID == "" {
		c.RunID = durableID("run")
	}
	if c.AttemptID == "" {
		c.AttemptID = durableID("attempt")
	}
}

func emitDurableCheckpoint(ctx context.Context, cfg *DurableRunConfig, sequence *uint64, boundary DurableBoundary, agent *Agent, history []RunItem, interruptions []*Interruption, usage Usage) error {
	if cfg == nil || cfg.Checkpoint == nil {
		return nil
	}
	cfg.normalize()
	*sequence++
	cp := DurableCheckpoint{
		SchemaVersion: DurableCheckpointSchemaVersion,
		RunID:         cfg.RunID, AttemptID: cfg.AttemptID, StepID: durableID("step"),
		Sequence: *sequence, Boundary: boundary, History: SnapshotRunItems(history),
		Interruptions: interruptions, Usage: usage, CreatedAt: time.Now().UTC(),
	}
	if agent != nil {
		cp.AgentName = agent.Name
	}
	if cfg.Children != nil {
		children := cfg.Children()
		cp.Children = &children
	}
	return cfg.Checkpoint(ctx, cp)
}

// RestoreRunItems converts persisted, provider-neutral checkpoint history back
// into replayable items. resolveAgent is optional; when supplied it restores
// item provenance by stable agent name.
func RestoreRunItems(items []LLMRunItemSnapshot, resolveAgent func(string) *Agent) ([]RunItem, error) {
	out := make([]RunItem, 0, len(items))
	for _, snap := range items {
		item := RunItem{}
		if resolveAgent != nil && snap.AgentName != "" {
			item.Agent = resolveAgent(snap.AgentName)
		}
		switch snap.Type {
		case "message":
			item.Type = RunItemMessage
			item.Message = &MessageOutput{Text: snap.MessageText, Phase: snap.MessagePhase, Images: append([]ImageAttachment(nil), snap.MessageImages...)}
		case "tool_call":
			item.Type = RunItemToolCall
			if snap.ToolCall == nil {
				return nil, fmt.Errorf("tool_call snapshot has no payload")
			}
			item.ToolCall = &ToolCallData{ID: snap.ToolCall.ID, Name: snap.ToolCall.Name, Input: cloneRaw(snap.ToolCall.Input)}
		case "tool_output":
			item.Type = RunItemToolOutput
			item.ToolOutput = cloneJSONValue(snap.ToolOutput)
		case "handoff_call":
			item.Type = RunItemHandoffCall
			item.HandoffCall = cloneJSONValue(snap.HandoffCall)
		case "handoff_output":
			item.Type = RunItemHandoffOutput
			item.HandoffOutput = cloneJSONValue(snap.HandoffOutput)
		case "reasoning":
			item.Type = RunItemReasoning
			if snap.Reasoning != nil {
				item.Reasoning = &ReasoningData{ID: snap.Reasoning.ID, Text: snap.Reasoning.Text, Signature: snap.Reasoning.Signature, RedactedData: snap.Reasoning.RedactedData, EncryptedContent: snap.Reasoning.EncryptedContent}
			}
		case "compaction":
			item.Type = RunItemCompaction
			if snap.Compaction != nil {
				item.Compaction = &CompactionData{ID: snap.Compaction.ID, Content: snap.Compaction.Content, EncryptedContent: snap.Compaction.EncryptedContent, CreatedBy: snap.Compaction.CreatedBy}
			}
		case "tool_approval":
			item.Type = RunItemToolApproval
			if snap.ToolApproval != nil {
				item.ToolApproval = &ToolApprovalData{ToolName: snap.ToolApproval.ToolName, Input: cloneRaw(snap.ToolApproval.Input), CallID: snap.ToolApproval.CallID, Approved: snap.ToolApproval.Approved}
			}
		default:
			return nil, fmt.Errorf("unknown durable run item type %q", snap.Type)
		}
		out = append(out, item)
	}
	return out, nil
}

func findRunAgent(root *Agent, name string) *Agent {
	if root == nil || name == "" {
		return nil
	}
	seen := map[*Agent]bool{}
	var visit func(*Agent) *Agent
	visit = func(a *Agent) *Agent {
		if a == nil || seen[a] {
			return nil
		}
		seen[a] = true
		if a.Name == name {
			return a
		}
		for _, h := range a.Handoffs {
			if h != nil {
				if found := visit(h.Agent); found != nil {
					return found
				}
			}
		}
		return nil
	}
	return visit(root)
}

func cloneRaw(v json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), v...) }
func cloneJSONValue[T any](v *T) *T {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}
