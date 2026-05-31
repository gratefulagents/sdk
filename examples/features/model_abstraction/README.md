# Model Abstraction

This example shows the provider-agnostic model interfaces: `Model`, `ModelProvider`, `MultiProvider`, and model prefix parsing.

Run it:

```sh
go test ./examples/features/model_abstraction
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Implement `agentsdk.Model` when you have a custom backend.
- Implement `agentsdk.ModelProvider` when model strings should resolve dynamically.
- Register providers in `agentsdk.NewMultiProvider(defaultPrefix)`.
- Use model names like `mock/fast`, `openai/gpt-4.1-mini`, or `anthropic/claude-haiku-4-5`.
- The runner strips the provider prefix before sending the concrete model name to the selected provider.

Runnable source: [model_abstraction_test.go](model_abstraction_test.go).
