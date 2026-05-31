# Observability

This example shows run hooks, progress tracking, event streams, lifecycle hooks, and tracing processors.

Run it:

```sh
go test ./examples/features/observability
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Implement `agentsdk.RunHooks` or `agentsdk.AgentHooks` for lifecycle callbacks.
- Use `agentsdk.NewProgressTracker` for in-memory progress snapshots.
- Use `agentsdk.NewEventStream(writer)` for JSONL content events.
- Use the SDK hook helpers to connect runner callbacks to tracker/event output.
- Implement `agentsdk.TracingProcessor` to export traces and spans.
- Pass hooks and tracing through `RunConfig`.
- Use `RecordToolResult`, `RecordLifecycleEvent`, `RecordSubagentProgress`, and `WriteResult` when a host needs direct progress snapshots outside the runner hook path.

Runnable source: [observability_test.go](observability_test.go).
