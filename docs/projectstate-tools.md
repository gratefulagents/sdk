# Project State Tools

Project state tools expose a `projectstate.Store` to an agent as durable task
and memory operations. They are host-neutral SDK tools; an application can add
them through `runtime.Builder` or by calling `projectstatetools.Tools` directly.

## Enablement

With the runtime builder, enable project state and provide a stable project ID:

```go
bundle, err := runtime.BuildToolBundle(ctx, runtime.Config{
	WorkDir:            workDir,
	EnableTools:        true,
	EnableProjectState: true,
	ProjectID:          "my-project",
	ProjectStateDir:    ".grateful/project-state",
})
```

To attach tools to a custom agent assembly:

```go
store, err := projectstate.NewFilesystemStore(projectstate.FilesystemOptions{
	StateDir:  ".grateful/project-state",
	ProjectID: "my-project",
	WorkDir:   workDir,
	Actor:     "assistant",
})
tools := projectstatetools.Tools(store, "assistant")
```

## Memory Tools

`memory_remember` writes or replaces a typed memory. Use it for durable facts,
preferences, procedures, and episode summaries that should survive context
compaction.

```json
{
  "content": "User prefers compact engineering answers.",
  "kind": "semantic",
  "scope": "user",
  "tags": ["preference", "style"]
}
```

`memory_recall` searches memories by query, kind, and tag. Recall is lexical by
default and becomes hybrid lexical plus embedding similarity when the store has
an embedder.

```json
{
  "query": "answer style",
  "tags": ["preference"],
  "limit": 5
}
```

`memory_list` lists memories without requiring a query. Use it for inspection,
audits, and workflows that need exact memory IDs.

```json
{
  "kinds": ["semantic"],
  "tags": ["preference"]
}
```

`memory_update` edits an existing memory by ID. Omitted fields preserve the
current value, while an empty array clears tags, task IDs, or file paths.

```json
{
  "id": "mem_abc123",
  "content": "User prefers compact answers with concrete file references.",
  "kind": "procedural",
  "tags": ["preference", "style"]
}
```

`memory_delete` removes one memory by ID.

```json
{
  "id": "mem_abc123"
}
```

`memory_stats` summarizes the current memory set by kind, scope, and tag. It
accepts the same kind and tag filters as `memory_list`.

```json
{
  "tags": ["preference"]
}
```

## Context Priming

`prime_context` returns a compact project-state summary for the start of a run.
Use it when an agent should see active tasks, recent session summaries, and
selected long-term memories before planning work.

```json
{
  "active_task_id": "task_abc123",
  "memory_limit": 8,
  "session_limit": 3
}
```
