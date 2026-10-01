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
tools := projectstatetools.Tools(store, "assistant", projectstatetools.WithWorkDir(workDir))
```

`WithWorkDir` lets the memory tools record the repository HEAD on save and
check cited files for staleness. Without it only the age rule applies.

## Memory Tools

Memories are short, typed, citable pieces of durable knowledge: a one-line
title (max 120 characters), a body (max 1500 characters), a kind, and optional
citations (workspace-relative paths or URLs). Kinds are `preference` (how the
user wants work done), `decision` (durable choices with rationale), `fact`
(non-obvious facts and gotchas), and `procedure` (repeatable how-tos). Progress
logs and PR changelogs do not belong in memory.

`memory_search` ranks memories by query (lexical by default, hybrid lexical plus
embedding similarity when the store has an embedder). Hits include a snippet,
a score rounded to two decimals, and `stale`/`stale_reason` when the memory
needs re-verification.

```json
{
  "query": "answer style",
  "kinds": ["preference"],
  "limit": 5
}
```

`memory_get` returns one memory in full plus `stale`/`stale_reason`, and records
the use. A memory is stale when a cited file no longer exists, cited files
changed since the commit it was verified at, or it has not been verified for
90 days.

```json
{ "id": "mem_abc123" }
```

`memory_save` creates a memory (no `id`) or replaces one (`id`; omitted fields
keep their current values). New memories that are near-duplicates of existing
ones are rejected with the candidate ids unless `allow_duplicate` is true. The
result carries a `warning` when the text reads like a progress log.

```json
{
  "kind": "preference",
  "title": "Compact engineering answers",
  "body": "The user wants compact answers with concrete file references.",
  "citations": [{ "path": "docs/style.md" }]
}
```

`memory_verify` marks a memory as re-checked against its sources (records the
current HEAD and resets the age). `memory_delete` removes an obsolete memory.

```json
{ "id": "mem_abc123", "reason": "superseded by mem_def456" }
```

## Context Priming

`prime_context` returns a deterministic project-state briefing for the start of
a run: the active task, ready and blocked work, and a memory index
(`- <id> [<kind>] <title>`) selected within a fixed byte budget. Preferences and
decisions are listed first; facts and procedures are ranked by use and recency.
Agents load full memories with `memory_get` and search for anything not listed.

```json
{
  "active_task_id": "task_abc123",
  "ready_limit": 8
}
```
