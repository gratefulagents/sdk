package projectstate

import (
	"context"
	"fmt"
	"strings"
)

func (s *FilesystemStore) PrimeContext(ctx context.Context, opts PrimeOptions) (string, error) {
	st, err := s.loadState(ctx)
	if err != nil {
		return "", err
	}
	opts.Actor = firstNonEmpty(opts.Actor, s.actor)
	if opts.ReadyLimit <= 0 {
		opts.ReadyLimit = 8
	}
	if opts.MemoryLimit <= 0 {
		opts.MemoryLimit = 8
	}

	var b strings.Builder
	b.WriteString("## Durable Project State\n")
	if st.project.ProjectID != "" {
		b.WriteString("Project: " + st.project.ProjectID + "\n")
	}
	if st.project.WorkDir != "" {
		b.WriteString("Workspace: " + st.project.WorkDir + "\n")
	}

	active := activeTask(st, opts)
	if active != nil {
		b.WriteString("\n### Active Task\n")
		writeTaskLine(&b, *active)
		if active.Description != "" {
			b.WriteString("  " + oneLine(active.Description, 220) + "\n")
		}
		if len(active.DependsOn) > 0 {
			b.WriteString("  Depends on: " + strings.Join(active.DependsOn, ", ") + "\n")
		}
	}

	ready := readyFromState(st, TaskFilter{Actor: opts.Actor, Limit: opts.ReadyLimit})
	if len(ready) > 0 {
		b.WriteString("\n### Ready Work\n")
		for _, task := range ready {
			writeTaskLine(&b, task)
		}
	}

	blocked := blockedTasks(st, 5)
	if len(blocked) > 0 {
		b.WriteString("\n### Blocked Work\n")
		for _, task := range blocked {
			writeTaskLine(&b, task)
		}
	}

	pinned, recent := memoriesForPrime(st, opts.MemoryLimit)
	if len(pinned) > 0 {
		b.WriteString("\n### Pinned Memories\n")
		for _, mem := range pinned {
			b.WriteString("- " + oneLine(mem.Content, 220) + memorySuffix(mem) + "\n")
		}
	}
	if len(recent) > 0 {
		b.WriteString("\n### Recent Memories\n")
		for _, mem := range recent {
			b.WriteString("- " + oneLine(mem.Content, 180) + memorySuffix(mem) + "\n")
		}
	}

	out := strings.TrimSpace(b.String())
	if out == "## Durable Project State" {
		out += "\nNo durable tasks or memories yet."
	}
	return out, nil
}

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
