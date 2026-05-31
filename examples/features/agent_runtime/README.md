# Agent Runtime

This example shows the core `Runner.Run` loop with an `Agent`, dynamic instructions, merged model settings, input items, final text, raw responses, and the last active agent.

Run it:

```sh
go test ./examples/features/agent_runtime
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Build an `agentsdk.Agent` with `Name`, `Model`, static `Instructions`, or `InstructionsFn`.
- Pass user messages as `[]agentsdk.RunItem`.
- Use `agentsdk.NewRunnerWithModel` for a concrete model or `agentsdk.NewRunnerWithProvider` for model-name resolution.
- Tune one run with `agentsdk.RunConfig`; run config model settings override or fill in the agent settings.
- Read `RunResult.FinalText()`, `RunResult.NewItems`, `RunResult.RawResponses`, `RunResult.Usage`, and `RunResult.LastAgent`.

Runnable source: [runtime_test.go](runtime_test.go).
