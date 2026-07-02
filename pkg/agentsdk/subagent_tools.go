package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BuildSubAgentTaskTools returns the model-facing sub-agent tool surface:
//
//   - subagent: spawn one task or a keyed DAG of tasks, sync or background
//   - subagent_status: progress, activity, and dependency-graph introspection
//   - subagent_control: steer (message) or cancel a running task
//
// All spawned tasks are managed by the scheduler: results are injected into
// the parent as soon as each task finishes, and the runner blocks final
// answers while tasks are still active.
func BuildSubAgentTaskTools(reg *SubAgentScheduler, defaultAgent string) []Tool {
	return []Tool{
		&subagentTool{registry: reg, defaultAgent: defaultAgent},
		&subagentStatusTool{registry: reg},
		&subagentControlTool{registry: reg},
	}
}

type subagentTaskToolBase struct{}

func (subagentTaskToolBase) IsEnabled(_ *RunContext) bool { return true }
func (subagentTaskToolBase) NeedsApproval() bool          { return false }
func (subagentTaskToolBase) TimeoutSeconds() int          { return 0 }

// --- subagent --------------------------------------------------------------

type subagentTool struct {
	subagentTaskToolBase
	registry     *SubAgentScheduler
	defaultAgent string
}

// subagentBatchTaskInput is one node of a keyed task DAG.
type subagentBatchTaskInput struct {
	Key                      string   `json:"key"`
	AgentName                string   `json:"agent_name"`
	Message                  string   `json:"message"`
	ToolAccess               string   `json:"tool_access"`
	DependsOn                []string `json:"depends_on"`
	DependencyPolicy         string   `json:"dependency_policy"`
	IncludeDependencyResults *bool    `json:"include_dependency_results"`
}

func (t *subagentTool) Name() string { return "subagent" }
func (t *subagentTool) Description() string {
	return "Delegate work to specialist sub-agents. Send one task (message) or a dependency DAG of tasks (tasks). mode=sync returns final results in this call; mode=background returns task ids immediately and results are delivered automatically as each task finishes."
}
func (t *subagentTool) IsReadOnly() bool { return false }
func (t *subagentTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"message": {
				"type": "string",
				"description": "Task packet for a single sub-agent. This is the ONLY context it receives: exact task, goal, relevant repo context, constraints/acceptance criteria, prior findings, expected output. Exactly one of message or tasks must be set."
			},
			"agent_name": {
				"type": "string",
				"description": "Specialist agent to run. Defaults to the configured default agent."
			},
			"mode": {
				"type": "string",
				"enum": ["sync", "background"],
				"description": "sync (default) blocks until the task(s) finish and returns results. background returns task ids immediately so you can keep working; results are injected automatically when each task finishes."
			},
			"tool_access": {
				"type": "string",
				"enum": ["full", "read-only"],
				"description": "Optional tool access override. Use read-only for explore/analysis tasks."
			},
			"depends_on": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Existing task ids that must finish before this single task starts. For a new multi-task DAG use tasks with logical keys instead."
			},
			"dependency_policy": {
				"type": "string",
				"enum": ["all_success", "all_terminal"],
				"description": "all_success (default) starts only if every dependency completed; all_terminal starts once dependencies are terminal even if some failed."
			},
			"include_dependency_results": {
				"type": "boolean",
				"description": "Append completed dependency outputs to the task message before it starts. Defaults to true."
			},
			"timeout_ms": {
				"type": "integer",
				"description": "sync mode only: maximum wait before returning current state. Omit or 0 to wait until done or the run is cancelled."
			},
			"tasks": {
				"type": "array",
				"description": "DAG of tasks to spawn in one call. Exactly one of message or tasks must be set. Each depends_on entry may reference another task's key or an existing task id.",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string", "description": "Logical key other tasks reference in depends_on."},
						"agent_name": {"type": "string", "description": "Specialist agent. Defaults to the configured default agent."},
						"message": {"type": "string", "description": "Self-contained task packet for this sub-agent."},
						"tool_access": {"type": "string", "enum": ["full", "read-only"], "description": "Optional tool access override."},
						"depends_on": {"type": "array", "items": {"type": "string"}, "description": "Keys in this batch or existing task ids that must finish first."},
						"dependency_policy": {"type": "string", "enum": ["all_success", "all_terminal"], "description": "Defaults to all_success."},
						"include_dependency_results": {"type": "boolean", "description": "Defaults to true."}
					},
					"required": ["key", "message"]
				}
			}
		}
	}`)
}

func (t *subagentTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		Message                  string                   `json:"message"`
		AgentName                string                   `json:"agent_name"`
		Mode                     string                   `json:"mode"`
		ToolAccess               string                   `json:"tool_access"`
		DependsOn                []string                 `json:"depends_on"`
		DependencyPolicy         string                   `json:"dependency_policy"`
		IncludeDependencyResults *bool                    `json:"include_dependency_results"`
		TimeoutMS                int64                    `json:"timeout_ms"`
		Tasks                    []subagentBatchTaskInput `json:"tasks"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
		}
	}
	background, err := parseSubagentMode(params.Mode)
	if err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	hasSingle := strings.TrimSpace(params.Message) != ""
	hasBatch := len(params.Tasks) > 0
	if hasSingle == hasBatch {
		return ToolResult{Content: "provide exactly one of 'message' (single task) or 'tasks' (task DAG)", IsError: true}, nil
	}

	if hasSingle {
		agentName := params.AgentName
		if agentName == "" {
			agentName = t.defaultAgent
		}
		if agentName == "" {
			return ToolResult{Content: "agent_name is required (no default agent configured)", IsError: true}, nil
		}
		taskID, err := t.registry.SpawnAsyncWithOptions(ctx, agentName, params.Message, SubAgentSpawnOptions{
			ToolAccessOverride:       toolAccessOverride(params.ToolAccess),
			DependsOn:                params.DependsOn,
			DependencyPolicy:         SubAgentDependencyPolicy(params.DependencyPolicy),
			IncludeDependencyResults: params.IncludeDependencyResults,
		})
		if err != nil {
			return ToolResult{Content: fmt.Sprintf("failed to spawn: %v", err), IsError: true}, nil
		}
		if background {
			resp, _ := json.Marshal(map[string]string{"task_id": taskID, "status": "pending", "agent": agentName})
			return ToolResult{Content: string(resp)}, nil
		}
		task, waitErr := t.registry.WaitForTask(ctx, taskID, params.TimeoutMS)
		if waitErr != nil {
			return joinedTaskResult(task, fmt.Sprintf("sub-agent wait failed: %v", waitErr), true), nil
		}
		if delivered, collectErr := t.registry.CollectResult(taskID); collectErr == nil && delivered != nil {
			task = delivered
		}
		if task.Status == SubAgentTaskFailed || task.Status == SubAgentTaskCancelled {
			return joinedTaskResult(task, task.Error, true), nil
		}
		return joinedTaskResult(task, "", false), nil
	}

	return t.executeBatch(ctx, params.Tasks, background, params.TimeoutMS)
}

// executeBatch spawns a keyed DAG of tasks and optionally waits for all of
// them (sync mode). depends_on entries resolve to in-batch keys first, then to
// already-existing task ids.
func (t *subagentTool) executeBatch(ctx context.Context, batch []subagentBatchTaskInput, background bool, timeoutMS int64) (ToolResult, error) {
	byKey := make(map[string]int, len(batch))
	for i, task := range batch {
		key := strings.TrimSpace(task.Key)
		if key == "" {
			return ToolResult{Content: fmt.Sprintf("tasks[%d].key is required", i), IsError: true}, nil
		}
		if _, exists := byKey[key]; exists {
			return ToolResult{Content: fmt.Sprintf("duplicate task key %q", key), IsError: true}, nil
		}
		if strings.TrimSpace(task.Message) == "" {
			return ToolResult{Content: fmt.Sprintf("tasks[%d].message is required", i), IsError: true}, nil
		}
		byKey[key] = i
	}
	for _, task := range batch {
		for _, dep := range task.DependsOn {
			dep = strings.TrimSpace(dep)
			if _, ok := byKey[dep]; ok {
				continue
			}
			if _, err := t.registry.GetStatus(dep); err != nil {
				return ToolResult{Content: fmt.Sprintf("task %q depends on %q, which is neither a key in this batch nor an existing task id", task.Key, dep), IsError: true}, nil
			}
		}
	}
	if cycle := findTaskGraphCycle(batch); len(cycle) > 0 {
		return ToolResult{Content: fmt.Sprintf("dependency cycle detected: %s", strings.Join(cycle, " -> ")), IsError: true}, nil
	}

	spawned := map[string]string{}
	type spawnSummary struct {
		Key       string   `json:"key"`
		TaskID    string   `json:"task_id"`
		Agent     string   `json:"agent"`
		DependsOn []string `json:"depends_on,omitempty"`
	}
	summaries := make([]spawnSummary, 0, len(batch))
	taskIDs := make([]string, 0, len(batch))

	for len(spawned) < len(batch) {
		progress := false
		for _, task := range batch {
			key := strings.TrimSpace(task.Key)
			if _, ok := spawned[key]; ok {
				continue
			}
			depIDs := make([]string, 0, len(task.DependsOn))
			ready := true
			for _, dep := range task.DependsOn {
				dep = strings.TrimSpace(dep)
				if depTaskID, ok := spawned[dep]; ok {
					depIDs = append(depIDs, depTaskID)
					continue
				}
				if _, inBatch := byKey[dep]; inBatch {
					// In-batch dependency not spawned yet.
					ready = false
					break
				}
				// Pre-validated existing task id.
				depIDs = append(depIDs, dep)
			}
			if !ready {
				continue
			}

			agentName := strings.TrimSpace(task.AgentName)
			if agentName == "" {
				agentName = t.defaultAgent
			}
			taskID, err := t.registry.SpawnAsyncWithOptions(ctx, agentName, task.Message, SubAgentSpawnOptions{
				ToolAccessOverride:       toolAccessOverride(task.ToolAccess),
				DependsOn:                depIDs,
				DependencyPolicy:         SubAgentDependencyPolicy(task.DependencyPolicy),
				IncludeDependencyResults: task.IncludeDependencyResults,
			})
			if err != nil {
				return ToolResult{Content: fmt.Sprintf("failed to spawn task %q: %v", key, err), IsError: true}, nil
			}
			spawned[key] = taskID
			taskIDs = append(taskIDs, taskID)
			summaries = append(summaries, spawnSummary{
				Key:       key,
				TaskID:    taskID,
				Agent:     agentName,
				DependsOn: depIDs,
			})
			progress = true
		}
		if !progress {
			return ToolResult{Content: "dependency graph could not be resolved", IsError: true}, nil
		}
	}

	if background {
		resp, _ := json.MarshalIndent(struct {
			Tasks []spawnSummary    `json:"tasks"`
			Map   map[string]string `json:"task_ids_by_key"`
		}{Tasks: summaries, Map: spawned}, "", "  ")
		return ToolResult{Content: string(resp)}, nil
	}

	tasks, waitErr := t.registry.WaitForTasks(ctx, taskIDs, timeoutMS)
	if waitErr == nil {
		markTasksDelivered(t.registry, taskIDs)
	}
	resp, _ := json.MarshalIndent(struct {
		Tasks        []spawnSummary    `json:"tasks"`
		Map          map[string]string `json:"task_ids_by_key"`
		Results      []joinedTaskJSON  `json:"results"`
		WaitComplete bool              `json:"wait_complete"`
		Error        string            `json:"error,omitempty"`
	}{Tasks: summaries, Map: spawned, Results: joinedTaskJSONs(tasks), WaitComplete: waitErr == nil, Error: errorString(waitErr)}, "", "  ")
	return ToolResult{Content: string(resp), IsError: waitErr != nil || hasFailedTasks(tasks)}, nil
}

// Final-join wiring: the runner blocks parent finalization while managed tasks
// are active and polls for early results at every turn boundary.

func (t *subagentTool) JoinSubAgentResults(ctx context.Context) ([]RunItem, error) {
	return joinSubAgentResults(ctx, t.registry)
}

func (t *subagentTool) HasPendingSubAgentFinalJoin() bool {
	return t.registry != nil && t.registry.HasPendingFinalJoinTasks()
}

func (t *subagentTool) PollSubAgentResults() []RunItem {
	if t.registry == nil {
		return nil
	}
	results, _ := t.registry.FinalJoinSnapshot()
	text := BuildSubAgentResultsContext(results)
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []RunItem{{
		Type:    RunItemMessage,
		Message: &MessageOutput{Text: text},
	}}
}

func parseSubagentMode(mode string) (background bool, err error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "sync":
		return false, nil
	case "background", "async":
		return true, nil
	default:
		return false, fmt.Errorf("invalid mode %q: use \"sync\" or \"background\"", mode)
	}
}

func toolAccessOverride(toolAccess string) ToolAccessLevel {
	if strings.TrimSpace(toolAccess) == "read-only" {
		return ToolAccessLevelReadOnly
	}
	return ""
}

// findTaskGraphCycle detects circular in-batch dependencies and returns the
// cycle path for the error message.
func findTaskGraphCycle(tasks []subagentBatchTaskInput) []string {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(tasks))
	inBatch := make(map[string]bool, len(tasks))
	depsByKey := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		key := strings.TrimSpace(task.Key)
		inBatch[key] = true
		depsByKey[key] = task.DependsOn
	}

	var stack []string
	var visit func(string) []string
	visit = func(key string) []string {
		switch state[key] {
		case visiting:
			for i, stacked := range stack {
				if stacked == key {
					return append(append([]string(nil), stack[i:]...), key)
				}
			}
			return []string{key, key}
		case visited:
			return nil
		}

		state[key] = visiting
		stack = append(stack, key)
		for _, dep := range depsByKey[key] {
			dep = strings.TrimSpace(dep)
			if dep == "" || !inBatch[dep] {
				continue
			}
			if cycle := visit(dep); len(cycle) > 0 {
				return cycle
			}
		}
		stack = stack[:len(stack)-1]
		state[key] = visited
		return nil
	}

	for _, task := range tasks {
		key := strings.TrimSpace(task.Key)
		if cycle := visit(key); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

// --- shared result/JSON helpers -------------------------------------------

type joinedTaskJSON struct {
	TaskID   string `json:"task_id"`
	Agent    string `json:"agent"`
	Status   string `json:"status"`
	Duration string `json:"duration,omitempty"`
	Result   string `json:"result,omitempty"`
	Error    string `json:"error,omitempty"`
}

func joinedTaskJSONs(tasks []SubAgentTask) []joinedTaskJSON {
	out := make([]joinedTaskJSON, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, joinedTaskJSON{
			TaskID:   task.ID,
			Agent:    task.AgentName,
			Status:   string(task.Status),
			Duration: task.Duration.String(),
			Result:   task.Result,
			Error:    task.Error,
		})
	}
	return out
}

func joinedTaskResult(task *SubAgentTask, errMsg string, isError bool) ToolResult {
	if task == nil {
		return ToolResult{Content: errMsg, IsError: isError}
	}
	content, _ := json.MarshalIndent(joinedTaskJSONs([]SubAgentTask{*task})[0], "", "  ")
	if errMsg != "" && task.Error == "" {
		var payload map[string]any
		_ = json.Unmarshal(content, &payload)
		payload["error"] = errMsg
		content, _ = json.MarshalIndent(payload, "", "  ")
	}
	return ToolResult{Content: string(content), IsError: isError}
}

func hasFailedTasks(tasks []SubAgentTask) bool {
	for _, task := range tasks {
		if task.Status == SubAgentTaskFailed || task.Status == SubAgentTaskCancelled {
			return true
		}
	}
	return false
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func joinSubAgentResults(ctx context.Context, registry *SubAgentScheduler) ([]RunItem, error) {
	if registry == nil {
		return nil, nil
	}
	results, err := registry.WaitForUndeliveredResults(ctx)
	if err != nil {
		return nil, err
	}
	text := BuildSubAgentResultsContext(results)
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	return []RunItem{{
		Type:    RunItemMessage,
		Message: &MessageOutput{Text: text},
	}}, nil
}

func markTasksDelivered(registry *SubAgentScheduler, taskIDs []string) {
	if registry == nil {
		return
	}
	for _, taskID := range uniqueNonEmptyTaskIDs(taskIDs) {
		_, _ = registry.CollectResult(taskID)
	}
}

// --- status snapshot helpers ---------------------------------------------

type subagentProgressSummary struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Pending   int `json:"pending"`
	Waiting   int `json:"waiting"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Cancelled int `json:"cancelled"`
}

type subagentProgressTask struct {
	TaskID            string   `json:"task_id"`
	Agent             string   `json:"agent"`
	Status            string   `json:"status"`
	Duration          string   `json:"duration,omitempty"`
	DependsOn         []string `json:"depends_on,omitempty"`
	WaitingOn         []string `json:"waiting_on,omitempty"`
	CurrentStep       string   `json:"current_step,omitempty"`
	LastTool          string   `json:"last_tool,omitempty"`
	FilesWritten      int      `json:"files_written,omitempty"`
	MessagesReceived  int      `json:"messages_received,omitempty"`
	LastParentMessage string   `json:"last_parent_message,omitempty"`
	ResultAvailable   bool     `json:"result_available,omitempty"`
	Error             string   `json:"error,omitempty"`
}

func buildSubagentProgressSnapshot(registry *SubAgentScheduler, taskIDs []string) (subagentProgressSummary, []subagentProgressTask, error) {
	var tasks []*SubAgentTask
	if len(taskIDs) > 0 {
		for _, taskID := range uniqueNonEmptyTaskIDs(taskIDs) {
			task, err := registry.GetStatus(taskID)
			if err != nil {
				return subagentProgressSummary{}, nil, err
			}
			tasks = append(tasks, task)
		}
	} else {
		tasks = registry.ListTasks()
	}

	summary := subagentProgressSummary{Total: len(tasks)}
	snapshot := make([]subagentProgressTask, 0, len(tasks))
	for _, task := range tasks {
		switch task.Status {
		case SubAgentTaskPending:
			summary.Pending++
			summary.Active++
		case SubAgentTaskWaiting:
			summary.Waiting++
			summary.Active++
		case SubAgentTaskRunning:
			summary.Running++
			summary.Active++
		case SubAgentTaskCompleted:
			summary.Completed++
		case SubAgentTaskFailed:
			summary.Failed++
		case SubAgentTaskCancelled:
			summary.Cancelled++
		default:
			if !task.IsTerminal() {
				summary.Active++
			}
		}

		snapshot = append(snapshot, subagentProgressTask{
			TaskID:            task.ID,
			Agent:             task.AgentName,
			Status:            string(task.Status),
			Duration:          subagentTaskDurationString(task),
			DependsOn:         append([]string(nil), task.DependsOn...),
			WaitingOn:         append([]string(nil), task.WaitingOn...),
			CurrentStep:       task.CurrentStep,
			LastTool:          task.LastTool,
			FilesWritten:      task.FilesWritten,
			MessagesReceived:  task.MessagesReceived,
			LastParentMessage: task.LastParentMessage,
			ResultAvailable:   task.IsTerminal() && task.Result != "",
			Error:             task.Error,
		})
	}
	return summary, snapshot, nil
}

func subagentTaskDurationString(task *SubAgentTask) string {
	if task == nil {
		return ""
	}
	if task.Duration > 0 {
		return task.Duration.String()
	}
	if task.StartedAt.IsZero() {
		return ""
	}
	return time.Since(task.StartedAt).Round(time.Millisecond).String()
}

func uniqueNonEmptyTaskIDs(taskIDs []string) []string {
	seen := make(map[string]struct{}, len(taskIDs))
	out := make([]string, 0, len(taskIDs))
	for _, taskID := range taskIDs {
		taskID = strings.TrimSpace(taskID)
		if taskID == "" {
			continue
		}
		if _, ok := seen[taskID]; ok {
			continue
		}
		seen[taskID] = struct{}{}
		out = append(out, taskID)
	}
	return out
}

// --- subagent_status --------------------------------------------------------

type subagentStatusTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *subagentStatusTool) Name() string { return "subagent_status" }
func (t *subagentStatusTool) Description() string {
	return "Inspect sub-agent tasks. detail=summary (default) returns per-task status and counts; detail=activity adds files read/written and recent tool calls; detail=graph returns the dependency DAG as nodes and edges."
}
func (t *subagentStatusTool) IsReadOnly() bool { return true }
func (t *subagentStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_ids": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Task ids to inspect. Omit for all tasks."
			},
			"detail": {
				"type": "string",
				"enum": ["summary", "activity", "graph"],
				"description": "summary (default): status snapshot. activity: per-task tool/file activity — use for evidence before steering. graph: dependency DAG."
			}
		}
	}`)
}

func (t *subagentStatusTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TaskIDs []string `json:"task_ids"`
		Detail  string   `json:"detail"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
		}
	}
	switch strings.ToLower(strings.TrimSpace(params.Detail)) {
	case "", "summary":
		summary, tasks, err := buildSubagentProgressSnapshot(t.registry, params.TaskIDs)
		if err != nil {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		resp, _ := json.MarshalIndent(struct {
			Summary subagentProgressSummary `json:"summary"`
			Tasks   []subagentProgressTask  `json:"tasks"`
		}{Summary: summary, Tasks: tasks}, "", "  ")
		return ToolResult{Content: string(resp)}, nil
	case "activity":
		return t.executeActivity(params.TaskIDs)
	case "graph":
		return t.executeGraph()
	default:
		return ToolResult{Content: fmt.Sprintf("invalid detail %q: use \"summary\", \"activity\", or \"graph\"", params.Detail), IsError: true}, nil
	}
}

func (t *subagentStatusTool) executeActivity(taskIDs []string) (ToolResult, error) {
	ids := uniqueNonEmptyTaskIDs(taskIDs)
	if len(ids) == 0 {
		for _, task := range t.registry.ListTasks() {
			ids = append(ids, task.ID)
		}
	}
	type taskActivity struct {
		TaskID   string                    `json:"task_id"`
		Agent    string                    `json:"agent"`
		Status   string                    `json:"status"`
		Duration string                    `json:"duration,omitempty"`
		Activity *SubAgentActivitySnapshot `json:"activity"`
	}
	out := make([]taskActivity, 0, len(ids))
	for _, taskID := range ids {
		task, err := t.registry.GetStatus(taskID)
		if err != nil {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		snap, err := t.registry.GetActivity(taskID, true)
		if err != nil {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		out = append(out, taskActivity{
			TaskID:   task.ID,
			Agent:    task.AgentName,
			Status:   string(task.Status),
			Duration: task.Duration.String(),
			Activity: snap,
		})
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return ToolResult{Content: string(b)}, nil
}

func (t *subagentStatusTool) executeGraph() (ToolResult, error) {
	tasks := t.registry.ListTasks()
	type node struct {
		ID                string   `json:"id"`
		Agent             string   `json:"agent"`
		Status            string   `json:"status"`
		DependsOn         []string `json:"depends_on,omitempty"`
		WaitingOn         []string `json:"waiting_on,omitempty"`
		MessagesReceived  int      `json:"messages_received,omitempty"`
		LastParentMessage string   `json:"last_parent_message,omitempty"`
	}
	type edge struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	nodes := make([]node, 0, len(tasks))
	var edges []edge
	for _, task := range tasks {
		nodes = append(nodes, node{
			ID:                task.ID,
			Agent:             task.AgentName,
			Status:            string(task.Status),
			DependsOn:         append([]string(nil), task.DependsOn...),
			WaitingOn:         append([]string(nil), task.WaitingOn...),
			MessagesReceived:  task.MessagesReceived,
			LastParentMessage: task.LastParentMessage,
		})
		for _, dep := range task.DependsOn {
			edges = append(edges, edge{From: dep, To: task.ID})
		}
	}
	resp, _ := json.MarshalIndent(struct {
		Nodes []node `json:"nodes"`
		Edges []edge `json:"edges"`
	}{Nodes: nodes, Edges: edges}, "", "  ")
	return ToolResult{Content: string(resp)}, nil
}

// --- subagent_control -------------------------------------------------------

type subagentControlTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *subagentControlTool) Name() string { return "subagent_control" }
func (t *subagentControlTool) Description() string {
	return "Steer or stop a running sub-agent task. action=message queues a short steering update the child receives before its next model turn; action=cancel stops the task."
}
func (t *subagentControlTool) IsReadOnly() bool { return false }
func (t *subagentControlTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["message", "cancel"],
				"description": "message: queue a steering update for the task. cancel: stop the task."
			},
			"task_id": {"type": "string", "description": "Task id to control."},
			"message": {"type": "string", "description": "Required for action=message: short, specific steering update. Do not resend the whole original task."}
		},
		"required": ["action", "task_id"]
	}`)
}

func (t *subagentControlTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		Action  string `json:"action"`
		TaskID  string `json:"task_id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	switch strings.ToLower(strings.TrimSpace(params.Action)) {
	case "message":
		if strings.TrimSpace(params.Message) == "" {
			return ToolResult{Content: "message is required for action=message", IsError: true}, nil
		}
		if err := t.registry.SendMessage(params.TaskID, params.Message); err != nil {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		resp, _ := json.Marshal(map[string]string{"task_id": params.TaskID, "status": "message_queued"})
		return ToolResult{Content: string(resp)}, nil
	case "cancel":
		if err := t.registry.Cancel(params.TaskID); err != nil {
			return ToolResult{Content: err.Error(), IsError: true}, nil
		}
		resp, _ := json.Marshal(map[string]string{"task_id": params.TaskID, "status": "cancellation_requested"})
		return ToolResult{Content: string(resp)}, nil
	default:
		return ToolResult{Content: fmt.Sprintf("invalid action %q: use \"message\" or \"cancel\"", params.Action), IsError: true}, nil
	}
}
