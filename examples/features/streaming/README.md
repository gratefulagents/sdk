# Streaming

This example shows both streaming surfaces: runner-level `RunStreamed` events and low-level `ModelStream` events.

Run it:

```sh
go test ./examples/features/streaming
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Use `runner.RunStreamed(ctx, agent, input, config)` to get a `StreamedRunResult`.
- Read `StreamedRunResult.Events` for live raw model deltas, completed run item events, content events, and typed sub-agent progress events.
- Handle `StreamEventSubAgent` when a host UI wants live sub-agent status without asking the parent model to poll.
- Call `FinalResult()` after the events channel closes to get the final `RunResult`.
- Call `Err()` after `FinalResult()` or after `Events` closes to inspect terminal stream errors.
- Use `Model.StreamResponse` and `agentsdk.NewModelStream` for lower-level provider streams.

Runnable source: [streaming_test.go](streaming_test.go).
