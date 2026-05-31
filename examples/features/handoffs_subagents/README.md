# Handoffs And Sub-Agents

This example shows two composition patterns: `NewHandoff` for transferring control to another agent, and `Agent.AsTool` for running a nested agent as a tool.

Run it:

```sh
go test ./examples/features/handoffs_subagents
```

Live test: uses OpenAI OAuth credentials at `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`) when present; missing credentials skip by default. Optional overrides: `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` (default `gpt-5.5`). Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) section for the full env table.

How to use this feature:

- Create specialist agents as normal `agentsdk.Agent` values.
- Add `agentsdk.NewHandoff(targetAgent)` to an agent's `Handoffs` list when the model should transfer control.
- Customize handoff tool names with `agentsdk.WithToolName`.
- Pass structured handoff arguments with `agentsdk.WithInputType`, and react to them with `agentsdk.WithOnHandoff` (receives the model's JSON input before control transfers).
- Gate a handoff dynamically with `agentsdk.WithIsEnabled` so it is hidden from the model when its predicate returns false.
- Shape the history the receiving agent sees with `agentsdk.WithInputFilter`; use the prebuilt `agentsdk.RemoveAllToolsHandoffInputFilter` to strip tool-call noise.
- Prepend `agentsdk.RecommendedHandoffPromptPrefix` (or wrap with `agentsdk.WithRecommendedHandoffInstructions`) so handoff-aware agents treat transfers as seamless.
- Use `specialist.AsTool(runner)` when the parent agent should call the specialist and then continue its own loop.
- Override an agent-as-tool's name/description with `agentsdk.WithAsToolName` / `agentsdk.WithAsToolDescription`, and post-process its result with `agentsdk.WithAsToolOutputExtractor` to return a compact or structured view to the parent.
- Control nested run budgets with `RunConfig.SubAgentMaxTurns`.

Runnable source: [handoffs_subagents_test.go](handoffs_subagents_test.go).
