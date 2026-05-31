# Costs

This example shows usage aggregation, runner response cost annotation, and provider cost helpers.

Run it:

```sh
go test ./examples/features/costs
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Return `agentsdk.Usage` from your model responses.
- Implement `CalculateCost(usage)` on custom models.
- Optionally implement `EstimateCost(usage) (float64, bool)` when pricing may be unknown.
- Read aggregate tokens from `RunResult.Usage`.
- Read per-response cost from `RunResult.RawResponses[i].CostUSD` and `CostKnown`.
- Use `sdkopenai.EstimateCost` or `sdkanthropic.CalculateCost` for built-in provider pricing.

Runnable source: [costs_test.go](costs_test.go).
