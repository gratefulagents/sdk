package projectstate

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// ActiveTask returns the task with activeTaskID when it exists, otherwise the
// first in_progress task (in SortTasks order) assigned to actor, or to anyone
// when actor is empty. It returns nil when there is none.
func ActiveTask(tasks []Task, activeTaskID, actor string) *Task {
	activeTaskID = strings.TrimSpace(activeTaskID)
	if activeTaskID != "" {
		for _, task := range tasks {
			if task.ID == activeTaskID {
				return cloneTaskPtr(task)
			}
		}
	}
	sorted := append([]Task(nil), tasks...)
	SortTasks(sorted)
	for _, task := range sorted {
		if task.Status == TaskStatusInProgress && (actor == "" || task.Assignee == actor) {
			return cloneTaskPtr(task)
		}
	}
	return nil
}

// ReadyFromTasks returns open tasks with no open blocker that match filter:
// labels must all be present; tasks assigned to someone else are excluded
// unless filter.IncludeAssigned (and always when they differ from
// filter.Assignee). The result is sorted with SortTasks and limited.
func ReadyFromTasks(tasks []Task, filter TaskFilter) []Task {
	byID := tasksByID(tasks)
	actor := firstNonEmpty(filter.Actor, filter.Assignee)
	var out []Task
	for _, task := range tasks {
		if task.Status != TaskStatusOpen {
			continue
		}
		if !matchesLabels(task.Labels, filter.Labels) {
			continue
		}
		if filter.Assignee != "" && task.Assignee != "" && task.Assignee != filter.Assignee {
			continue
		}
		if !filter.IncludeAssigned && task.Assignee != "" && task.Assignee != actor {
			continue
		}
		if hasOpenBlocker(byID, task) {
			continue
		}
		out = append(out, cloneTask(task))
	}
	SortTasks(out)
	return limitTasks(out, filter.Limit)
}

// BlockedFromTasks returns open or blocked tasks that still wait on an open
// (or missing) dependency, sorted with SortTasks and limited.
func BlockedFromTasks(tasks []Task, max int) []Task {
	byID := tasksByID(tasks)
	var out []Task
	for _, task := range tasks {
		if task.Status != TaskStatusOpen && task.Status != TaskStatusBlocked {
			continue
		}
		if hasOpenBlocker(byID, task) {
			out = append(out, cloneTask(task))
		}
	}
	SortTasks(out)
	return limitTasks(out, max)
}

// RecomputeBlocks rebuilds every task's Blocks list from the DependsOn edges.
func RecomputeBlocks(tasks map[string]Task) {
	for id, task := range tasks {
		task.Blocks = nil
		tasks[id] = task
	}
	for id, task := range tasks {
		for _, depID := range task.DependsOn {
			dep, ok := tasks[depID]
			if !ok {
				continue
			}
			dep.Blocks = appendUnique(dep.Blocks, id)
			tasks[depID] = dep
		}
	}
	for id, task := range tasks {
		if len(task.Blocks) > 1 {
			sort.Strings(task.Blocks)
			tasks[id] = task
		}
	}
}

// ApplyTaskPatch applies the non-nil fields of patch to task and stamps
// UpdatedAt (and ClosedAt when the status changes) with now.
func ApplyTaskPatch(task *Task, patch TaskPatch, now time.Time) {
	if patch.Title != nil {
		task.Title = strings.TrimSpace(*patch.Title)
	}
	if patch.Description != nil {
		task.Description = strings.TrimSpace(*patch.Description)
	}
	if patch.Type != nil {
		task.Type = NormalizeTaskType(*patch.Type)
	}
	if patch.Status != nil {
		task.Status = NormalizeTaskStatus(*patch.Status)
		if task.Status == TaskStatusClosed {
			task.ClosedAt = &now
		} else {
			task.ClosedAt = nil
		}
	}
	if patch.Priority != nil {
		task.Priority = NormalizePriority(*patch.Priority)
	}
	if patch.Assignee != nil {
		task.Assignee = strings.TrimSpace(*patch.Assignee)
	}
	if patch.ReplaceLabels {
		task.Labels = uniqueNonEmpty(patch.Labels)
	}
	if patch.Metadata != nil {
		task.Metadata = cloneRaw(*patch.Metadata)
	}
	task.UpdatedAt = now
}

func NormalizeTaskType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TaskTypeBug:
		return TaskTypeBug
	case TaskTypeFeature, "feat":
		return TaskTypeFeature
	case TaskTypeChore:
		return TaskTypeChore
	case TaskTypeEpic:
		return TaskTypeEpic
	default:
		return TaskTypeTask
	}
}

func NormalizeTaskStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case TaskStatusInProgress, "in-progress", "claimed":
		return TaskStatusInProgress
	case TaskStatusBlocked:
		return TaskStatusBlocked
	case TaskStatusClosed, "done", "completed":
		return TaskStatusClosed
	case TaskStatusDeferred:
		return TaskStatusDeferred
	default:
		return TaskStatusOpen
	}
}

// NormalizePriority clamps priority to [0,4].
func NormalizePriority(value int) int {
	if value < 0 {
		return 0
	}
	if value > 4 {
		return 4
	}
	return value
}

// SortTasks orders tasks by status, priority, UpdatedAt desc, then ID.
func SortTasks(tasks []Task) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].Status != tasks[j].Status {
			return tasks[i].Status < tasks[j].Status
		}
		if tasks[i].Priority != tasks[j].Priority {
			return tasks[i].Priority < tasks[j].Priority
		}
		if !tasks[i].UpdatedAt.Equal(tasks[j].UpdatedAt) {
			return tasks[i].UpdatedAt.After(tasks[j].UpdatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
}

func tasksByID(tasks []Task) map[string]Task {
	byID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	return byID
}

func hasOpenBlocker(tasks map[string]Task, task Task) bool {
	for _, depID := range task.DependsOn {
		dep, ok := tasks[depID]
		if !ok || dep.Status != TaskStatusClosed {
			return true
		}
	}
	return false
}

func limitTasks(tasks []Task, limit int) []Task {
	if limit > 0 && len(tasks) > limit {
		tasks = tasks[:limit]
	}
	out := make([]Task, len(tasks))
	copy(out, tasks)
	return out
}

func writeTaskLine(b *strings.Builder, task Task) {
	status := task.Status
	if status == "" {
		status = TaskStatusOpen
	}
	fmt.Fprintf(b, "- %s [P%d %s] %s\n", task.ID, task.Priority, status, oneLine(task.Title, 180))
}

func oneLine(value string, max int) string {
	value = collapseSpace(value)
	if max <= 3 || len(value) <= max {
		return value
	}
	cut := max - 3
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "..."
}
