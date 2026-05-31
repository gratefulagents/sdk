# Context Compaction

This example shows local history compaction helpers, model-specific default thresholds, and extraction of generated compaction summaries.

Run it:

```sh
go test ./examples/features/context_compaction
```

How to use this feature:

- Use `agentsdk.DefaultCompactionConfig()` for long-running sessions.
- Set `RunConfig.CompactionConfig` to enable proactive compaction during normal runs.
- Use `RunConfig.CompactionRecorder` and `RunConfig.CompactionFailureReporter` to observe compaction boundaries.
- Use `agentsdk.MaybeCompactRunItems` when you need to compact stored history yourself.
- Use `agentsdk.CompactionDefaultsForModel(model)` to pick a trigger and target based on the context window.
- Use `agentsdk.ExtractCompactionSummary(items)` to find the latest local compaction summary.

Runnable source: [context_compaction_test.go](context_compaction_test.go).
