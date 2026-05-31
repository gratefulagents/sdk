package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkmemory "github.com/gratefulagents/sdk/pkg/agentsdk/memory"
)

// Tool exposes the semantic memory system to agents.
type Tool struct {
	store     sdkmemory.Store
	namespace string
	sourceRun string
	repoURL   string
}

// New creates a memory tool scoped to a namespace and source run.
func New(store sdkmemory.Store, namespace, sourceRun, repoURL string) *Tool {
	return &Tool{store: store, namespace: namespace, sourceRun: sourceRun, repoURL: repoURL}
}

func (t *Tool) Name() string { return "Memory" }

func (t *Tool) Description() string {
	return "Store and retrieve persistent semantic memories across agent runs. Actions: store (save a memory), search (find similar memories), list (browse memories), delete (remove a memory)."
}

func (t *Tool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["store", "search", "list", "delete"],
				"description": "The action to perform"
			},
			"content": {
				"type": "string",
				"description": "Memory content to store, or search query text"
			},
			"tags": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Tags for categorization or filtering"
			},
			"id": {
				"type": "string",
				"description": "Memory ID (required for delete)"
			},
			"limit": {
				"type": "integer",
				"description": "Max results to return (default 10 for search, 50 for list)"
			}
		},
		"required": ["action"]
	}`)
}

func (t *Tool) IsReadOnly() bool { return false }

func (t *Tool) IsEnabled(ctx *agentsdk.RunContext) bool {
	return ctx == nil || ctx.ToolAccessLevel != agentsdk.ToolAccessLevelReadOnly
}

func (t *Tool) NeedsApproval() bool { return false }

func (t *Tool) TimeoutSeconds() int { return 0 }

type input struct {
	Action  string   `json:"action"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	ID      string   `json:"id"`
	Limit   int      `json:"limit"`
}

func (t *Tool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid input: %v", err), IsError: true}, nil
	}
	if t.store == nil {
		return agentsdk.ToolResult{Content: "memory store is not configured", IsError: true}, nil
	}

	switch in.Action {
	case "store":
		return t.executeStore(ctx, in)
	case "search":
		return t.executeSearch(ctx, in)
	case "list":
		return t.executeList(ctx, in)
	case "delete":
		return t.executeDelete(ctx, in)
	default:
		return agentsdk.ToolResult{Content: fmt.Sprintf("Unknown action: %q. Use store, search, list, or delete.", in.Action), IsError: true}, nil
	}
}

func (t *Tool) executeStore(ctx context.Context, in input) (agentsdk.ToolResult, error) {
	if in.Content == "" {
		return agentsdk.ToolResult{Content: "content is required for store action", IsError: true}, nil
	}

	metadata := json.RawMessage(`{}`)
	if t.repoURL != "" {
		// Marshalling a map[string]string with a single ASCII key cannot fail,
		// so we ignore the error and fall back to the empty-object literal.
		metadata, _ = json.Marshal(map[string]string{"repo": t.repoURL})
	}

	mem, err := t.store.Store(ctx, t.namespace, in.Content, in.Tags, t.sourceRun, metadata)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to store memory: %v", err), IsError: true}, nil
	}
	out, _ := json.Marshal(mem)
	return agentsdk.ToolResult{Content: string(out)}, nil
}

func (t *Tool) executeSearch(ctx context.Context, in input) (agentsdk.ToolResult, error) {
	if in.Content == "" {
		return agentsdk.ToolResult{Content: "content is required for search action (used as query)", IsError: true}, nil
	}

	memories, err := t.store.Search(ctx, t.namespace, in.Content, in.Tags, in.Limit)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to search memories: %v", err), IsError: true}, nil
	}
	if len(memories) == 0 {
		return agentsdk.ToolResult{Content: "No matching memories found."}, nil
	}
	out, _ := json.Marshal(memories)
	return agentsdk.ToolResult{Content: string(out)}, nil
}

func (t *Tool) executeList(ctx context.Context, in input) (agentsdk.ToolResult, error) {
	memories, err := t.store.List(ctx, t.namespace, in.Tags, in.Limit)
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to list memories: %v", err), IsError: true}, nil
	}
	if len(memories) == 0 {
		return agentsdk.ToolResult{Content: "No memories found."}, nil
	}
	out, _ := json.Marshal(memories)
	return agentsdk.ToolResult{Content: string(out)}, nil
}

func (t *Tool) executeDelete(ctx context.Context, in input) (agentsdk.ToolResult, error) {
	if in.ID == "" {
		return agentsdk.ToolResult{Content: "id is required for delete action", IsError: true}, nil
	}

	id, err := uuid.Parse(strings.TrimSpace(in.ID))
	if err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Invalid memory ID: %v", err), IsError: true}, nil
	}
	if err := t.store.Delete(ctx, t.namespace, id); err != nil {
		return agentsdk.ToolResult{Content: fmt.Sprintf("Failed to delete memory: %v", err), IsError: true}, nil
	}
	return agentsdk.ToolResult{Content: fmt.Sprintf("Memory %s deleted.", id)}, nil
}
