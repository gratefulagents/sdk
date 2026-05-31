# ChatLoop

This example runs the SDK `ChatLoop` with an in-memory session adapter and the shared live provider runner.

Run it:

```sh
go test ./examples/features/chatloop
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.
