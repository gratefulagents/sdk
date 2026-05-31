# Tools

This example shows `FunctionTool`, JSON tool input schemas, tool execution, tool output items, approval interruptions, and `WrapWithPolicy`.

Run it:

```sh
go test ./examples/features/tools
```

Live tests: OpenAI OAuth (`TestFunctionToolExample`, `TestToolApprovalAndPolicyWrapperExample`) when credentials are present; missing credentials skip by default. `TestToolRegistryExample` is offline. The OpenAI-OAuth test path uses `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`); optional overrides `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL`. Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) for the full env table.

How to use this feature:

- Implement `agentsdk.Tool` directly or use `agentsdk.FunctionTool`.
- Set `Schema` to the JSON object schema the model should use for tool calls.
- Mark harmless tools with `ReadOnly: true`.
- Set `Approval: true` or wrap a tool with `agentsdk.WrapWithPolicy(tool, approval, timeoutSeconds)` to pause before execution.
- Inspect `RunResult.Interruption` when a tool needs approval.
- Use `RunConfig.ToolAccessLevel` to expose all tools or only read-only/control-flow tools.

Runnable source: [tools_test.go](tools_test.go).
