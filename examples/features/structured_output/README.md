# Structured Output

This example shows `OutputSchema`, custom parsing with `ParseFn`, and structured values in `RunResult.FinalOutput`.

Run it:

```sh
go test ./examples/features/structured_output
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Create a schema with `agentsdk.NewOutputSchema(name, schemaJSON)`.
- Set `schema.ParseFn` when you want typed Go output instead of generic JSON.
- Attach it to `Agent.OutputType`.
- Read the parsed value from `RunResult.FinalOutput`.
- Use `OutputSchema.Validate` directly when you need to validate raw model text outside a run.
- The runner adds portable schema instructions for every provider and validates/parses the final output.
- OpenAI-compatible providers also forward native JSON schema response-format parameters when the selected API mode supports them.

Runnable source: [structured_output_test.go](structured_output_test.go).
