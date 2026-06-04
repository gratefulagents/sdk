package projectstate

import (
	"fmt"
	"strings"
)

func activeTask(st *state, opts PrimeOptions) *Task {
	if opts.ActiveTaskID != "" {
		if task, ok := st.tasks[opts.ActiveTaskID]; ok {
			return cloneTaskPtr(task)
		}
	}
	for _, task := range st.tasks {
		if task.Status == TaskStatusInProgress && (opts.Actor == "" || task.Assignee == opts.Actor) {
			return cloneTaskPtr(task)
		}
	}
	return nil
}

func readyFromState(st *state, filter TaskFilter) []Task {
	var tasks []Task
	for _, task := range st.tasks {
		if task.Status != TaskStatusOpen {
			continue
		}
		if !matchesLabels(task.Labels, filter.Labels) {
			continue
		}
		actor := firstNonEmpty(filter.Actor, filter.Assignee)
		if filter.Assignee != "" && task.Assignee != "" && task.Assignee != filter.Assignee {
			continue
		}
		if !filter.IncludeAssigned && task.Assignee != "" && task.Assignee != actor {
			continue
		}
		if hasOpenBlocker(st, task) {
			continue
		}
		tasks = append(tasks, cloneTask(task))
	}
	sortTasks(tasks)
	return limitTasks(tasks, filter.Limit)
}

func blockedTasks(st *state, limit int) []Task {
	var tasks []Task
	for _, task := range st.tasks {
		if task.Status != TaskStatusOpen && task.Status != TaskStatusBlocked {
			continue
		}
		if hasOpenBlocker(st, task) {
			tasks = append(tasks, cloneTask(task))
		}
	}
	sortTasks(tasks)
	return limitTasks(tasks, limit)
}

func memoriesForPrime(st *state, limit int) (pinned, recent []Memory) {
	for _, mem := range st.memories {
		switch mem.Kind {
		case MemoryKindPinned:
			pinned = append(pinned, cloneMemory(mem))
		default:
			recent = append(recent, cloneMemory(mem))
		}
	}
	sortMemoriesForPrime(pinned)
	sortMemoriesForPrime(recent)
	if limit > 0 && len(pinned) > limit {
		pinned = pinned[:limit]
	}
	remaining := limit
	if remaining > 0 {
		remaining -= len(pinned)
	}
	if remaining <= 0 {
		remaining = limit
	}
	if remaining > 0 && len(recent) > remaining {
		recent = recent[:remaining]
	}
	return pinned, recent
}

func sortMemoriesForPrime(memories []Memory) {
	for i := 0; i < len(memories); i++ {
		for j := i + 1; j < len(memories); j++ {
			if memories[j].UpdatedAt.After(memories[i].UpdatedAt) {
				memories[i], memories[j] = memories[j], memories[i]
			}
		}
	}
}

func writeTaskLine(b *strings.Builder, task Task) {
	status := task.Status
	if status == "" {
		status = TaskStatusOpen
	}
	fmt.Fprintf(b, "- %s [P%d %s] %s\n", task.ID, task.Priority, status, oneLine(task.Title, 180))
}

func memorySuffix(mem Memory) string {
	var parts []string
	if mem.Kind != "" {
		parts = append(parts, mem.Kind)
	}
	if len(mem.Tags) > 0 {
		parts = append(parts, strings.Join(mem.Tags, ","))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if max > 0 && len(value) > max {
		return value[:max-3] + "..."
	}
	return value
}
