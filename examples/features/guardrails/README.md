# Guardrails

This example shows input guardrails, output guardrails, tool input guardrails, tool output guardrails, and tripwire errors.
It also includes simple `tripwire-*` demo guardrails for deterministic examples.

Run it:

```sh
go test ./examples/features/guardrails
```

Live tests: OpenAI OAuth (first two tests) when credentials are present; missing credentials skip by default. The third test (`TestGuardrailFunctionsExample`) is offline. The OpenAI-OAuth test path uses `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`); optional overrides `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL`. Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) for the full env table.

How to use this feature:

- Add `InputGuardrails` to an agent to inspect user input before accepting final output.
- Add `OutputGuardrails` to inspect the final output before returning it.
- Add `RunConfig.ToolInputGuardrails` to inspect tool-call arguments before execution.
- Add `RunConfig.ToolOutputGuardrails` to inspect tool results after execution.
- Return `GuardrailResult{TripwireTriggered: true}` to stop the run with a typed tripwire error.
- Use `errors.As` with `InputGuardrailTripwireTriggered`, `OutputGuardrailTripwireTriggered`, `ToolInputGuardrailTripwireTriggered`, or `ToolOutputGuardrailTripwireTriggered`.

Runnable source: [guardrails_test.go](guardrails_test.go).
