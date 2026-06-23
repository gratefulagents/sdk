package agentsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// BuildSubAgentTaskTools returns the standard async sub-agent suite.
// Every spawned task is managed by the SDK: the parent run stays open while
// tasks are active, status is streamed by the runtime, and each terminal
// result is injected into the parent at the next turn boundary as soon as it
// is available (incremental delivery), with a blocking final-join before
// finalization for any remaining tasks. Blocking wait/poll tools are intentionally
// not part of the default model-facing suite; hosts can instantiate them
// directly for diagnostics or explicit user-driven status checks.
func BuildSubAgentTaskTools(reg *SubAgentScheduler, defaultAgent string) []Tool {
	return []Tool{
		&spawnSubagentTaskTool{registry: reg, defaultAgent: defaultAgent},
		&runSubagentTaskTool{registry: reg, defaultAgent: defaultAgent},
		&spawnSubagentGraphTool{registry: reg, defaultAgent: defaultAgent},
		&listSubagentTasksTool{registry: reg},
		&getSubagentTaskStatusTool{registry: reg},
		&getSubagentActivityTool{registry: reg},
		&getSubagentTaskGraphTool{registry: reg},
		&sendMessageToSubagentTaskTool{registry: reg},
		&cancelSubagentTaskTool{registry: reg},
	}
}

type subagentTaskToolBase struct{}

func (subagentTaskToolBase) IsEnabled(_ *RunContext) bool { return true }
func (subagentTaskToolBase) NeedsApproval() bool          { return false }
func (subagentTaskToolBase) TimeoutSeconds() int          { return 0 }

// --- spawn_subagent_task -------------------------------------------------

type spawnSubagentTaskTool struct {
	subagentTaskToolBase
	registry     *SubAgentScheduler
	defaultAgent string
}

func (t *spawnSubagentTaskTool) Name() string { return "spawn_subagent_task" }
func (t *spawnSubagentTaskTool) Description() string {
	return "Launch a managed specialist sub-agent task. Returns a task_id immediately so the parent can monitor, report progress, and steer it; the SDK prevents final answers while managed tasks are still active."
}
func (t *spawnSubagentTaskTool) IsReadOnly() bool { return false }
func (t *spawnSubagentTaskTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"agent_name": {
				"type": "string",
				"description": "Name of the specialist agent to spawn (e.g. 'researcher')."
			},
			"message": {
				"type": "string",
				"description": "The task instructions for the sub-agent. This is the ONLY context the sub-agent receives. Send a compact task packet: exact task, goal, only the relevant repo context, constraints/acceptance criteria, prior findings this task depends on, and expected output."
			},
			"tool_access": {
				"type": "string",
				"enum": ["full", "read-only"],
				"description": "Optional tool access level override. Use 'read-only' for explore/analysis tasks."
			},
			"depends_on": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional task IDs that must finish before this task starts. Use spawn_subagent_graph for a full DAG with logical task keys."
			},
			"dependency_policy": {
				"type": "string",
				"enum": ["all_success", "all_terminal"],
				"description": "How to treat dependency outcomes. all_success waits for every dependency to complete successfully; all_terminal starts once all dependencies are terminal."
			},
			"include_dependency_results": {
				"type": "boolean",
				"description": "Whether completed dependency outputs should be appended to this task's message before it starts. Defaults to true."
			}
		},
		"required": ["agent_name", "message"]
	}`)
}
func (t *spawnSubagentTaskTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		AgentName                string   `json:"agent_name"`
		Message                  string   `json:"message"`
		ToolAccess               string   `json:"tool_access"`
		DependsOn                []string `json:"depends_on"`
		DependencyPolicy         string   `json:"dependency_policy"`
		IncludeDependencyResults *bool    `json:"include_dependency_results"`
		AutoJoin                 bool     `json:"auto_join"` // Deprecated and ignored; all tasks are managed.
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
		}
	}
	if params.AgentName == "" {
		params.AgentName = t.defaultAgent
	}
	if params.AgentName == "" || params.Message == "" {
		return ToolResult{Content: "agent_name and message are required (provide a non-empty task description in 'message')", IsError: true}, nil
	}
	var accessOverride ToolAccessLevel
	if params.ToolAccess == "read-only" {
		accessOverride = ToolAccessLevelReadOnly
	}
	taskID, err := t.registry.SpawnAsyncWithOptions(ctx, params.AgentName, params.Message, SubAgentSpawnOptions{
		ToolAccessOverride:       accessOverride,
		DependsOn:                params.DependsOn,
		DependencyPolicy:         SubAgentDependencyPolicy(params.DependencyPolicy),
		IncludeDependencyResults: params.IncludeDependencyResults,
		AutoJoin:                 true,
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("failed to spawn: %v", err), IsError: true}, nil
	}
	resp, _ := json.Marshal(map[string]string{"task_id": taskID, "status": "pending", "agent": params.AgentName})
	return ToolResult{Content: string(resp)}, nil
}

func (t *spawnSubagentTaskTool) JoinSubAgentResults(ctx context.Context) ([]RunItem, error) {
	return joinSubAgentResults(ctx, t.registry)
}

func (t *spawnSubagentTaskTool) HasPendingSubAgentFinalJoin() bool {
	return t.registry != nil && t.registry.HasPendingFinalJoinTasks()
}

// PollSubAgentResults drains terminal, undelivered managed task results
// without blocking. The runner calls this at every turn boundary so the parent
// receives each result as soon as the task finishes (incremental delivery).
func (t *spawnSubagentTaskTool) PollSubAgentResults() []RunItem {
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

// --- run_subagent_task ---------------------------------------------------

type runSubagentTaskTool struct {
	subagentTaskToolBase
	registry     *SubAgentScheduler
	defaultAgent string
}

func (t *runSubagentTaskTool) Name() string { return "run_subagent_task" }
func (t *runSubagentTaskTool) Description() string {
	return "Run a specialist sub-agent and return its final result in this same tool call. Use this as the default when the parent needs the delegated result before continuing."
}
func (t *runSubagentTaskTool) IsReadOnly() bool { return false }
func (t *runSubagentTaskTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"agent_name": {
				"type": "string",
				"description": "Name of the specialist agent to run. Defaults to the configured default agent if omitted."
			},
			"message": {
				"type": "string",
				"description": "Self-contained task packet for the sub-agent."
			},
			"tool_access": {
				"type": "string",
				"enum": ["full", "read-only"],
				"description": "Optional tool access level override. Use 'read-only' for explore/analysis tasks."
			},
			"depends_on": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional async task IDs that must finish before this task starts."
			},
			"dependency_policy": {
				"type": "string",
				"enum": ["all_success", "all_terminal"],
				"description": "How to treat dependency outcomes. Defaults to all_success."
			},
			"include_dependency_results": {
				"type": "boolean",
				"description": "Whether completed dependency outputs should be appended to this task's message before it starts. Defaults to true."
			},
			"timeout_ms": {
				"type": "integer",
				"description": "Optional maximum time to wait for completion. Omit or set 0 to wait until the run context is cancelled."
			}
		},
		"required": ["message"]
	}`)
}
func (t *runSubagentTaskTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		AgentName                string   `json:"agent_name"`
		Message                  string   `json:"message"`
		ToolAccess               string   `json:"tool_access"`
		DependsOn                []string `json:"depends_on"`
		DependencyPolicy         string   `json:"dependency_policy"`
		IncludeDependencyResults *bool    `json:"include_dependency_results"`
		TimeoutMS                int64    `json:"timeout_ms"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
		}
	}
	if params.AgentName == "" {
		params.AgentName = t.defaultAgent
	}
	if params.AgentName == "" || strings.TrimSpace(params.Message) == "" {
		return ToolResult{Content: "agent_name and message are required (provide a non-empty task description in 'message')", IsError: true}, nil
	}
	var accessOverride ToolAccessLevel
	if params.ToolAccess == "read-only" {
		accessOverride = ToolAccessLevelReadOnly
	}
	taskID, err := t.registry.SpawnAsyncWithOptions(ctx, params.AgentName, params.Message, SubAgentSpawnOptions{
		ToolAccessOverride:       accessOverride,
		DependsOn:                params.DependsOn,
		DependencyPolicy:         SubAgentDependencyPolicy(params.DependencyPolicy),
		IncludeDependencyResults: params.IncludeDependencyResults,
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("failed to run sub-agent: %v", err), IsError: true}, nil
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

// --- spawn_subagent_graph ------------------------------------------------

type spawnSubagentGraphTool struct {
	subagentTaskToolBase
	registry     *SubAgentScheduler
	defaultAgent string
}

type subagentGraphTaskInput struct {
	Key                      string   `json:"key"`
	AgentName                string   `json:"agent_name"`
	Message                  string   `json:"message"`
	ToolAccess               string   `json:"tool_access"`
	DependsOn                []string `json:"depends_on"`
	DependencyPolicy         string   `json:"dependency_policy"`
	IncludeDependencyResults *bool    `json:"include_dependency_results"`
	AutoJoin                 bool     `json:"auto_join"` // Deprecated and ignored; all tasks are managed.
}

func (t *spawnSubagentGraphTool) Name() string { return "spawn_subagent_graph" }
func (t *spawnSubagentGraphTool) Description() string {
	return "Launch a DAG of managed async sub-agent tasks in one call. Each task uses a logical 'key'; depends_on references those keys. The parent run stays responsible for monitoring active tasks before finalization."
}
func (t *spawnSubagentGraphTool) IsReadOnly() bool { return false }
func (t *spawnSubagentGraphTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"tasks": {
				"type": "array",
				"description": "DAG nodes to spawn. Use stable, short keys so dependencies are easy to read.",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string", "description": "Logical key used by other tasks' depends_on lists."},
						"agent_name": {"type": "string", "description": "Specialist agent name. Defaults to the configured default agent if omitted."},
						"message": {"type": "string", "description": "Self-contained task packet for this sub-agent."},
						"tool_access": {"type": "string", "enum": ["full", "read-only"], "description": "Optional tool access override."},
						"depends_on": {"type": "array", "items": {"type": "string"}, "description": "Logical task keys that must finish before this task starts."},
						"dependency_policy": {"type": "string", "enum": ["all_success", "all_terminal"], "description": "Defaults to all_success."},
						"include_dependency_results": {"type": "boolean", "description": "Defaults to true."}
					},
					"required": ["key", "message"]
				}
			},
			"return_when": {
				"type": "string",
				"enum": ["spawned", "all_complete"],
				"description": "spawned returns task IDs immediately while keeping the parent responsible for monitoring before finalization. all_complete waits until every graph task is terminal and returns their results."
			},
			"wait": {
				"type": "boolean",
				"description": "Deprecated alias for return_when=all_complete."
			},
			"timeout_ms": {
				"type": "integer",
				"description": "Optional maximum time to wait when return_when=all_complete. Omit or set 0 to wait until the run context is cancelled."
			}
		},
		"required": ["tasks"]
	}`)
}
func (t *spawnSubagentGraphTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		Tasks      []subagentGraphTaskInput `json:"tasks"`
		ReturnWhen string                   `json:"return_when"`
		Wait       bool                     `json:"wait"`
		TimeoutMS  int64                    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if len(params.Tasks) == 0 {
		return ToolResult{Content: "tasks is required and must be non-empty", IsError: true}, nil
	}

	byKey := make(map[string]int, len(params.Tasks))
	for i, task := range params.Tasks {
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
	for _, task := range params.Tasks {
		for _, dep := range task.DependsOn {
			if _, ok := byKey[strings.TrimSpace(dep)]; !ok {
				return ToolResult{Content: fmt.Sprintf("task %q depends on unknown key %q", task.Key, dep), IsError: true}, nil
			}
		}
	}
	if cycle := findSubagentGraphCycle(params.Tasks); len(cycle) > 0 {
		return ToolResult{Content: fmt.Sprintf("dependency cycle detected in subagent graph: %s", strings.Join(cycle, " -> ")), IsError: true}, nil
	}

	spawned := map[string]string{}
	type spawnSummary struct {
		Key       string   `json:"key"`
		TaskID    string   `json:"task_id"`
		Agent     string   `json:"agent"`
		DependsOn []string `json:"depends_on,omitempty"`
	}
	summaries := make([]spawnSummary, 0, len(params.Tasks))
	taskIDs := make([]string, 0, len(params.Tasks))

	for len(spawned) < len(params.Tasks) {
		progress := false
		for _, task := range params.Tasks {
			key := strings.TrimSpace(task.Key)
			if _, ok := spawned[key]; ok {
				continue
			}
			depIDs := make([]string, 0, len(task.DependsOn))
			ready := true
			for _, dep := range task.DependsOn {
				depTaskID, ok := spawned[strings.TrimSpace(dep)]
				if !ok {
					ready = false
					break
				}
				depIDs = append(depIDs, depTaskID)
			}
			if !ready {
				continue
			}

			agentName := strings.TrimSpace(task.AgentName)
			if agentName == "" {
				agentName = t.defaultAgent
			}
			var accessOverride ToolAccessLevel
			if task.ToolAccess == "read-only" {
				accessOverride = ToolAccessLevelReadOnly
			}
			taskID, err := t.registry.SpawnAsyncWithOptions(ctx, agentName, task.Message, SubAgentSpawnOptions{
				ToolAccessOverride:       accessOverride,
				DependsOn:                depIDs,
				DependencyPolicy:         SubAgentDependencyPolicy(task.DependencyPolicy),
				IncludeDependencyResults: task.IncludeDependencyResults,
				AutoJoin:                 true,
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

	returnWhen := strings.ToLower(strings.TrimSpace(params.ReturnWhen))
	waitForGraph := params.Wait || returnWhen == "all_complete"
	if waitForGraph {
		tasks, waitErr := t.registry.WaitForTasks(ctx, taskIDs, params.TimeoutMS)
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

	resp, _ := json.MarshalIndent(struct {
		Tasks []spawnSummary    `json:"tasks"`
		Map   map[string]string `json:"task_ids_by_key"`
	}{Tasks: summaries, Map: spawned}, "", "  ")
	return ToolResult{Content: string(resp)}, nil
}

func findSubagentGraphCycle(tasks []subagentGraphTaskInput) []string {
	const (
		unvisited = 0
		visiting  = 1
		visited   = 2
	)
	state := make(map[string]int, len(tasks))
	depsByKey := make(map[string][]string, len(tasks))
	for _, task := range tasks {
		key := strings.TrimSpace(task.Key)
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
			if dep == "" {
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

// --- list_subagent_tasks -------------------------------------------------

type listSubagentTasksTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *listSubagentTasksTool) Name() string { return "list_subagent_tasks" }
func (t *listSubagentTasksTool) Description() string {
	return "List all async sub-agent tasks with their current status, duration, and progress."
}
func (t *listSubagentTasksTool) IsReadOnly() bool { return true }
func (t *listSubagentTasksTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (t *listSubagentTasksTool) Execute(_ context.Context, _ json.RawMessage, _ string) (ToolResult, error) {
	return ToolResult{Content: t.registry.MarshalTaskList()}, nil
}

// --- get_subagent_task_status --------------------------------------------

type getSubagentTaskStatusTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *getSubagentTaskStatusTool) Name() string { return "get_subagent_task_status" }
func (t *getSubagentTaskStatusTool) Description() string {
	return "Get the current status of a specific async sub-agent task."
}
func (t *getSubagentTaskStatusTool) IsReadOnly() bool { return true }
func (t *getSubagentTaskStatusTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "Task ID returned by spawn_subagent_task."}
		},
		"required": ["task_id"]
	}`)
}
func (t *getSubagentTaskStatusTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	task, err := t.registry.GetStatus(params.TaskID)
	if err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	// Echo only a preview of the original task packet: the parent already has
	// the full message in its own context, so repeating it wastes tokens.
	view := *task
	view.Message = Truncate(task.Message, 240)
	b, _ := json.MarshalIndent(view, "", "  ")
	return ToolResult{Content: string(b)}, nil
}

// --- cancel_subagent_task ------------------------------------------------

type cancelSubagentTaskTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *cancelSubagentTaskTool) Name() string { return "cancel_subagent_task" }
func (t *cancelSubagentTaskTool) Description() string {
	return "Cancel a running async sub-agent task."
}
func (t *cancelSubagentTaskTool) IsReadOnly() bool { return false }
func (t *cancelSubagentTaskTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "Task ID to cancel."}
		},
		"required": ["task_id"]
	}`)
}
func (t *cancelSubagentTaskTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if err := t.registry.Cancel(params.TaskID); err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	return ToolResult{Content: fmt.Sprintf("Task %s cancellation requested.", params.TaskID)}, nil
}

// --- get_subagent_activity -----------------------------------------------

type getSubagentActivityTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *getSubagentActivityTool) Name() string { return "get_subagent_activity" }
func (t *getSubagentActivityTool) Description() string {
	return "Get detailed activity for a sub-agent task: files read, files written, current tool, and recent tool calls."
}
func (t *getSubagentActivityTool) IsReadOnly() bool { return true }
func (t *getSubagentActivityTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "Task ID to get activity for."},
			"include_recent": {"type": "boolean", "description": "Include recent tool call history (default: true)."}
		},
		"required": ["task_id"]
	}`)
}
func (t *getSubagentActivityTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TaskID        string `json:"task_id"`
		IncludeRecent *bool  `json:"include_recent"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	includeRecent := true
	if params.IncludeRecent != nil {
		includeRecent = *params.IncludeRecent
	}
	task, err := t.registry.GetStatus(params.TaskID)
	if err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	snap, err := t.registry.GetActivity(params.TaskID, includeRecent)
	if err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	result := struct {
		TaskID   string                    `json:"task_id"`
		Agent    string                    `json:"agent"`
		Status   string                    `json:"status"`
		Duration string                    `json:"duration,omitempty"`
		Activity *SubAgentActivitySnapshot `json:"activity"`
	}{
		TaskID:   task.ID,
		Agent:    task.AgentName,
		Status:   string(task.Status),
		Duration: task.Duration.String(),
		Activity: snap,
	}
	b, _ := json.MarshalIndent(result, "", "  ")
	return ToolResult{Content: string(b)}, nil
}

// --- get_subagent_task_graph --------------------------------------------

type getSubagentTaskGraphTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *getSubagentTaskGraphTool) Name() string { return "get_subagent_task_graph" }
func (t *getSubagentTaskGraphTool) Description() string {
	return "Return the current async sub-agent DAG as nodes and dependency edges."
}
func (t *getSubagentTaskGraphTool) IsReadOnly() bool { return true }
func (t *getSubagentTaskGraphTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type": "object", "properties": {}}`)
}
func (t *getSubagentTaskGraphTool) Execute(_ context.Context, _ json.RawMessage, _ string) (ToolResult, error) {
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

// --- wait_for_subagent_progress ------------------------------------------

type waitForSubagentProgressTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *waitForSubagentProgressTool) Name() string { return "wait_for_subagent_progress" }
func (t *waitForSubagentProgressTool) Description() string {
	return "Wait for a sub-agent state change or heartbeat timeout, then return a compact status snapshot the parent can report to the user. Does not collect final results."
}
func (t *waitForSubagentProgressTool) IsReadOnly() bool { return true }
func (t *waitForSubagentProgressTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"timeout_ms": {"type": "integer", "description": "Maximum time to wait before returning a heartbeat snapshot. Defaults to 30000."},
			"task_ids": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Optional task IDs to include in the returned snapshot. Omit to include all sub-agent tasks."
			}
		}
	}`)
}
func (t *waitForSubagentProgressTool) Execute(ctx context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TimeoutMS int64    `json:"timeout_ms"`
		TaskIDs   []string `json:"task_ids"`
	}
	if len(input) > 0 {
		if err := json.Unmarshal(input, &params); err != nil {
			return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
		}
	}
	if params.TimeoutMS <= 0 {
		params.TimeoutMS = 30000
	}

	start := time.Now()
	changedTask, err := t.registry.WaitForAny(ctx, params.TimeoutMS)
	waitedMS := time.Since(start).Milliseconds()
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("wait error: %v", err), IsError: true}, nil
	}

	summary, tasks, snapshotErr := buildSubagentProgressSnapshot(t.registry, params.TaskIDs)
	if snapshotErr != nil {
		return ToolResult{Content: snapshotErr.Error(), IsError: true}, nil
	}

	reason := "heartbeat"
	changedTaskID := ""
	if changedTask != nil {
		reason = "change"
		changedTaskID = changedTask.ID
	}
	resp, _ := json.MarshalIndent(struct {
		Changed       bool                    `json:"changed"`
		Reason        string                  `json:"reason"`
		ChangedTaskID string                  `json:"changed_task_id,omitempty"`
		WaitedMS      int64                   `json:"waited_ms"`
		TimeoutMS     int64                   `json:"timeout_ms"`
		Summary       subagentProgressSummary `json:"summary"`
		Tasks         []subagentProgressTask  `json:"tasks"`
	}{Changed: changedTask != nil, Reason: reason, ChangedTaskID: changedTaskID, WaitedMS: waitedMS, TimeoutMS: params.TimeoutMS, Summary: summary, Tasks: tasks}, "", "  ")
	return ToolResult{Content: string(resp)}, nil
}

// --- send_message_to_subagent_task --------------------------------------

type sendMessageToSubagentTaskTool struct {
	subagentTaskToolBase
	registry *SubAgentScheduler
}

func (t *sendMessageToSubagentTaskTool) Name() string { return "send_message_to_subagent_task" }
func (t *sendMessageToSubagentTaskTool) Description() string {
	return "Send a steering message to an active async sub-agent task. The child receives it before its next model turn. Use for clarifications, constraints, corrections, or updated findings."
}
func (t *sendMessageToSubagentTaskTool) IsReadOnly() bool { return false }
func (t *sendMessageToSubagentTaskTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"task_id": {"type": "string", "description": "Task ID to message."},
			"message": {"type": "string", "description": "Short, specific steering update. Do not resend the whole original task unless necessary."}
		},
		"required": ["task_id", "message"]
	}`)
}
func (t *sendMessageToSubagentTaskTool) Execute(_ context.Context, input json.RawMessage, _ string) (ToolResult, error) {
	var params struct {
		TaskID  string `json:"task_id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &params); err != nil {
		return ToolResult{Content: fmt.Sprintf("invalid input: %v", err), IsError: true}, nil
	}
	if err := t.registry.SendMessage(params.TaskID, params.Message); err != nil {
		return ToolResult{Content: err.Error(), IsError: true}, nil
	}
	resp, _ := json.Marshal(map[string]string{
		"task_id": params.TaskID,
		"status":  "message_queued",
	})
	return ToolResult{Content: string(resp)}, nil
}
