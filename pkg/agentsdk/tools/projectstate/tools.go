package projectstatetools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	"github.com/gratefulagents/sdk/pkg/agentsdk/projectstate"
)

func Tools(store projectstate.Store, actor string) []agentsdk.Tool {
	if store == nil {
		return nil
	}
	base := baseTool{store: store, actor: strings.TrimSpace(actor)}
	return []agentsdk.Tool{
		&taskCreateTool{baseTool: base},
		&taskReadyTool{baseTool: base},
		&taskShowTool{baseTool: base},
		&taskUpdateTool{baseTool: base},
		&taskClaimTool{baseTool: base},
		&taskCloseTool{baseTool: base},
		&taskCommentTool{baseTool: base},
		&taskLinkTool{baseTool: base},
		&memoryRememberTool{baseTool: base},
		&memoryRecallTool{baseTool: base},
		&memoryListTool{baseTool: base},
		&memoryUpdateTool{baseTool: base},
		&memoryDeleteTool{baseTool: base},
		&memoryStatsTool{baseTool: base},
		&primeContextTool{baseTool: base},
	}
}

type baseTool struct {
	store projectstate.Store
	actor string
}

func (t baseTool) IsEnabled(*agentsdk.RunContext) bool { return t.store != nil }
func (t baseTool) NeedsApproval() bool                 { return false }
func (t baseTool) TimeoutSeconds() int                 { return 0 }

type taskCreateTool struct{ baseTool }

func (t *taskCreateTool) Name() string { return "task_create" }
func (t *taskCreateTool) Description() string {
	return "Create a durable project task in the filesystem event log. Use for work that should survive sessions and context compaction."
}
func (t *taskCreateTool) IsReadOnly() bool { return false }
func (t *taskCreateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {"type": "string"},
			"description": {"type": "string"},
			"type": {"type": "string", "enum": ["task", "bug", "feature", "chore", "epic"]},
			"priority": {"type": "integer", "minimum": 0, "maximum": 4},
			"assignee": {"type": "string"},
			"depends_on": {"type": "array", "items": {"type": "string"}},
			"labels": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["title"]
	}`)
}
func (t *taskCreateTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Type        string   `json:"type"`
		Priority    *int     `json:"priority"`
		Assignee    string   `json:"assignee"`
		DependsOn   []string `json:"depends_on"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	priority := 2
	if in.Priority != nil {
		priority = *in.Priority
	}
	task, err := t.store.CreateTask(ctx, projectstate.CreateTaskInput{
		Title:       in.Title,
		Description: in.Description,
		Type:        in.Type,
		Priority:    priority,
		Assignee:    in.Assignee,
		DependsOn:   in.DependsOn,
		Labels:      in.Labels,
	})
	return jsonToolResult(task, err), nil
}

type taskReadyTool struct{ baseTool }

func (t *taskReadyTool) Name() string { return "task_ready" }
func (t *taskReadyTool) Description() string {
	return "List ready durable project tasks: open tasks with no open blockers."
}
func (t *taskReadyTool) IsReadOnly() bool { return true }
func (t *taskReadyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"limit": {"type": "integer"},
			"assignee": {"type": "string"},
			"labels": {"type": "array", "items": {"type": "string"}},
			"include_assigned": {"type": "boolean"}
		}
	}`)
}
func (t *taskReadyTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		Limit           int      `json:"limit"`
		Assignee        string   `json:"assignee"`
		Labels          []string `json:"labels"`
		IncludeAssigned bool     `json:"include_assigned"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	tasks, err := t.store.ReadyTasks(ctx, projectstate.TaskFilter{Actor: t.actor, Assignee: in.Assignee, Labels: in.Labels, Limit: in.Limit, IncludeAssigned: in.IncludeAssigned})
	return jsonToolResult(tasks, err), nil
}

type taskShowTool struct{ baseTool }

func (t *taskShowTool) Name() string { return "task_show" }
func (t *taskShowTool) Description() string {
	return "Show one durable project task with comments and dependencies."
}
func (t *taskShowTool) IsReadOnly() bool { return true }
func (t *taskShowTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
}
func (t *taskShowTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.GetTask(ctx, in.ID)
	return jsonToolResult(task, err), nil
}

type taskUpdateTool struct{ baseTool }

func (t *taskUpdateTool) Name() string { return "task_update" }
func (t *taskUpdateTool) Description() string {
	return "Update durable project task fields."
}
func (t *taskUpdateTool) IsReadOnly() bool { return false }
func (t *taskUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"id":{"type":"string"},
			"title":{"type":"string"},
			"description":{"type":"string"},
			"type":{"type":"string"},
			"status":{"type":"string"},
			"priority":{"type":"integer","minimum":0,"maximum":4},
			"assignee":{"type":"string"},
			"labels":{"type":"array","items":{"type":"string"}}
		},
		"required":["id"]
	}`)
}
func (t *taskUpdateTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID          string   `json:"id"`
		Title       *string  `json:"title"`
		Description *string  `json:"description"`
		Type        *string  `json:"type"`
		Status      *string  `json:"status"`
		Priority    *int     `json:"priority"`
		Assignee    *string  `json:"assignee"`
		Labels      []string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.UpdateTask(ctx, in.ID, projectstate.TaskPatch{
		Title:         in.Title,
		Description:   in.Description,
		Type:          in.Type,
		Status:        in.Status,
		Priority:      in.Priority,
		Assignee:      in.Assignee,
		Labels:        in.Labels,
		ReplaceLabels: in.Labels != nil,
	})
	return jsonToolResult(task, err), nil
}

type taskClaimTool struct{ baseTool }

func (t *taskClaimTool) Name() string { return "task_claim" }
func (t *taskClaimTool) Description() string {
	return "Atomically claim a durable project task for the current agent."
}
func (t *taskClaimTool) IsReadOnly() bool { return false }
func (t *taskClaimTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"actor":{"type":"string"}},"required":["id"]}`)
}
func (t *taskClaimTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID    string `json:"id"`
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.ClaimTask(ctx, in.ID, firstNonEmpty(in.Actor, t.actor))
	return jsonToolResult(task, err), nil
}

type taskCloseTool struct{ baseTool }

func (t *taskCloseTool) Name() string { return "task_close" }
func (t *taskCloseTool) Description() string {
	return "Close a durable project task with an optional reason."
}
func (t *taskCloseTool) IsReadOnly() bool { return false }
func (t *taskCloseTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"reason":{"type":"string"}},"required":["id"]}`)
}
func (t *taskCloseTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	task, err := t.store.CloseTask(ctx, in.ID, in.Reason)
	return jsonToolResult(task, err), nil
}

type taskCommentTool struct{ baseTool }

func (t *taskCommentTool) Name() string { return "task_comment" }
func (t *taskCommentTool) Description() string {
	return "Add a durable comment to a project task."
}
func (t *taskCommentTool) IsReadOnly() bool { return false }
func (t *taskCommentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"body":{"type":"string"},"actor":{"type":"string"}},"required":["id","body"]}`)
}
func (t *taskCommentTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID    string `json:"id"`
		Body  string `json:"body"`
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	comment, err := t.store.AddComment(ctx, in.ID, firstNonEmpty(in.Actor, t.actor), in.Body)
	return jsonToolResult(comment, err), nil
}

type taskLinkTool struct{ baseTool }

func (t *taskLinkTool) Name() string { return "task_link" }
func (t *taskLinkTool) Description() string {
	return "Add or remove a dependency between durable project tasks."
}
func (t *taskLinkTool) IsReadOnly() bool { return false }
func (t *taskLinkTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"},"depends_on":{"type":"string"},"action":{"type":"string","enum":["add","remove"]}},"required":["id","depends_on"]}`)
}
func (t *taskLinkTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID        string `json:"id"`
		DependsOn string `json:"depends_on"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	var err error
	if strings.EqualFold(in.Action, "remove") {
		err = t.store.RemoveDependency(ctx, in.ID, in.DependsOn)
	} else {
		err = t.store.AddDependency(ctx, in.ID, in.DependsOn)
	}
	return jsonToolResult(map[string]string{"id": in.ID, "depends_on": in.DependsOn, "action": firstNonEmpty(in.Action, "add")}, err), nil
}

type memoryRememberTool struct{ baseTool }

func (t *memoryRememberTool) Name() string { return "memory_remember" }
func (t *memoryRememberTool) Description() string {
	return "Store a durable typed project memory in the filesystem event log."
}
func (t *memoryRememberTool) IsReadOnly() bool { return false }
func (t *memoryRememberTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"id":{"type":"string"},
			"content":{"type":"string"},
			"kind":{"type":"string","enum":["pinned","semantic","episodic","procedural"]},
			"scope":{"type":"string","enum":["project","user","task","file"]},
			"tags":{"type":"array","items":{"type":"string"}},
			"task_ids":{"type":"array","items":{"type":"string"}},
			"file_paths":{"type":"array","items":{"type":"string"}}
		},
		"required":["content"]
	}`)
}
func (t *memoryRememberTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.UpsertMemoryInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	mem, err := t.store.UpsertMemory(ctx, in)
	return jsonToolResult(mem, err), nil
}

type memoryRecallTool struct{ baseTool }

func (t *memoryRecallTool) Name() string { return "memory_recall" }
func (t *memoryRecallTool) Description() string {
	return "Search durable typed project memories."
}
func (t *memoryRecallTool) IsReadOnly() bool { return true }
func (t *memoryRecallTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"kinds":{"type":"array","items":{"type":"string"}},"tags":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"}},"required":["query"]}`)
}
func (t *memoryRecallTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.MemoryFilter
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	memories, err := t.store.SearchMemories(ctx, in)
	return jsonToolResult(memories, err), nil
}

type memoryListTool struct{ baseTool }

func (t *memoryListTool) Name() string { return "memory_list" }
func (t *memoryListTool) Description() string {
	return "List durable typed project memories."
}
func (t *memoryListTool) IsReadOnly() bool { return true }
func (t *memoryListTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"kinds":{"type":"array","items":{"type":"string"}},"tags":{"type":"array","items":{"type":"string"}},"limit":{"type":"integer"}}}`)
}
func (t *memoryListTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.MemoryFilter
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	memories, err := t.store.ListMemories(ctx, in)
	return jsonToolResult(memories, err), nil
}

type memoryUpdateTool struct{ baseTool }

func (t *memoryUpdateTool) Name() string { return "memory_update" }
func (t *memoryUpdateTool) Description() string {
	return "Update an existing durable typed project memory by id. Omitted fields keep their current values."
}
func (t *memoryUpdateTool) IsReadOnly() bool { return false }
func (t *memoryUpdateTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{
			"id":{"type":"string"},
			"content":{"type":"string"},
			"kind":{"type":"string","enum":["pinned","semantic","episodic","procedural"]},
			"scope":{"type":"string","enum":["project","user","task","file"]},
			"tags":{"type":"array","items":{"type":"string"}},
			"task_ids":{"type":"array","items":{"type":"string"}},
			"file_paths":{"type":"array","items":{"type":"string"}},
			"source_run":{"type":"string"}
		},
		"required":["id"]
	}`)
}
func (t *memoryUpdateTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID        string   `json:"id"`
		Content   *string  `json:"content"`
		Kind      *string  `json:"kind"`
		Scope     *string  `json:"scope"`
		Tags      []string `json:"tags"`
		TaskIDs   []string `json:"task_ids"`
		FilePaths []string `json:"file_paths"`
		SourceRun *string  `json:"source_run"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	existing, err := memoryByID(ctx, t.store, in.ID)
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	update := projectstate.UpsertMemoryInput{
		ID:        existing.ID,
		Kind:      existing.Kind,
		Scope:     existing.Scope,
		Content:   existing.Content,
		Tags:      existing.Tags,
		TaskIDs:   existing.TaskIDs,
		FilePaths: existing.FilePaths,
		SourceRun: existing.SourceRun,
		Metadata:  existing.Metadata,
	}
	if in.Content != nil {
		update.Content = *in.Content
	}
	if in.Kind != nil {
		update.Kind = *in.Kind
	}
	if in.Scope != nil {
		update.Scope = *in.Scope
	}
	if in.Tags != nil {
		update.Tags = in.Tags
	}
	if in.TaskIDs != nil {
		update.TaskIDs = in.TaskIDs
	}
	if in.FilePaths != nil {
		update.FilePaths = in.FilePaths
	}
	if in.SourceRun != nil {
		update.SourceRun = *in.SourceRun
	}
	mem, err := t.store.UpsertMemory(ctx, update)
	return jsonToolResult(mem, err), nil
}

type memoryDeleteTool struct{ baseTool }

func (t *memoryDeleteTool) Name() string { return "memory_delete" }
func (t *memoryDeleteTool) Description() string {
	return "Delete one durable typed project memory by id."
}
func (t *memoryDeleteTool) IsReadOnly() bool { return false }
func (t *memoryDeleteTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`)
}
func (t *memoryDeleteTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	err := t.store.DeleteMemory(ctx, in.ID)
	return jsonToolResult(map[string]string{"id": strings.TrimSpace(in.ID), "status": "deleted"}, err), nil
}

type memoryStatsTool struct{ baseTool }

func (t *memoryStatsTool) Name() string { return "memory_stats" }
func (t *memoryStatsTool) Description() string {
	return "Summarize durable typed project memory counts by kind, scope, and tag."
}
func (t *memoryStatsTool) IsReadOnly() bool { return true }
func (t *memoryStatsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"kinds":{"type":"array","items":{"type":"string"}},"tags":{"type":"array","items":{"type":"string"}}}}`)
}
func (t *memoryStatsTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.MemoryFilter
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	memories, err := t.store.ListMemories(ctx, in)
	if err != nil {
		return jsonToolResult(nil, err), nil
	}
	stats := memoryStats{
		Total:   len(memories),
		ByKind:  map[string]int{},
		ByScope: map[string]int{},
		ByTag:   map[string]int{},
	}
	for _, mem := range memories {
		stats.ByKind[mem.Kind]++
		stats.ByScope[mem.Scope]++
		for _, tag := range mem.Tags {
			tag = strings.TrimSpace(tag)
			if tag != "" {
				stats.ByTag[tag]++
			}
		}
	}
	return jsonToolResult(stats, nil), nil
}

type memoryStats struct {
	Total   int            `json:"total"`
	ByKind  map[string]int `json:"by_kind"`
	ByScope map[string]int `json:"by_scope"`
	ByTag   map[string]int `json:"by_tag"`
}

func memoryByID(ctx context.Context, store projectstate.Store, id string) (projectstate.Memory, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return projectstate.Memory{}, fmt.Errorf("memory id is required")
	}
	memories, err := store.ListMemories(ctx, projectstate.MemoryFilter{})
	if err != nil {
		return projectstate.Memory{}, err
	}
	for _, mem := range memories {
		if mem.ID == id {
			return mem, nil
		}
	}
	return projectstate.Memory{}, fmt.Errorf("memory %q not found", id)
}

type primeContextTool struct{ baseTool }

func (t *primeContextTool) Name() string { return "prime_context" }
func (t *primeContextTool) Description() string {
	return "Build compact durable project context from tasks and memories for session start or compaction recovery."
}
func (t *primeContextTool) IsReadOnly() bool { return true }
func (t *primeContextTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"active_task_id":{"type":"string"},"ready_limit":{"type":"integer"},"memory_limit":{"type":"integer"}}}`)
}
func (t *primeContextTool) Execute(ctx context.Context, raw json.RawMessage, _ string) (agentsdk.ToolResult, error) {
	var in projectstate.PrimeOptions
	if err := json.Unmarshal(raw, &in); err != nil {
		return errorResult("Invalid input: %v", err), nil
	}
	in.Actor = firstNonEmpty(in.Actor, t.actor)
	text, err := t.store.PrimeContext(ctx, in)
	if err != nil {
		return errorResult("%v", err), nil
	}
	return agentsdk.ToolResult{Content: text}, nil
}

func jsonToolResult(value any, err error) agentsdk.ToolResult {
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return agentsdk.ToolResult{Content: err.Error(), IsError: true}
	}
	return agentsdk.ToolResult{Content: string(data)}
}

func errorResult(format string, args ...any) agentsdk.ToolResult {
	return agentsdk.ToolResult{Content: fmt.Sprintf(format, args...), IsError: true}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
