# Policy Primitives

This example shows the lightweight runtime policy package exposed at `pkg/agentsdk/policy`.

Run it:

```sh
go test ./examples/features/policy
```

Live tests: OpenAI OAuth (`TestRunnerToolPolicyExample`) when credentials are present; missing credentials skip by default. `TestPolicyExample` is offline. The OpenAI-OAuth test path uses `$HOME/.codex/auth.json` (or `OPENAI_OAUTH_AUTH_JSON_PATH`); optional overrides `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL`. Set `GRATEFUL_LIVE_TESTS=required` to fail when credentials are missing, or `GRATEFUL_LIVE_TESTS=skip` to skip live provider calls. See the top-level README [Running tests](../../../README.md#running-tests) for the full env table.

How to use this feature:

- Use `policy.RuntimePolicy` to describe the resolved runtime surface for a session.
- Use `policy.NormalizePermissionMode` for compatible defaults.
- Use `policy.NewToolPolicy` to derive tool mutability checks.
- Use `PermissionMode.AllowsWriteTools` and `PermissionMode.AllowsMCPTool(readOnlyHint)` when building your own tool registry.
- Use `RunConfig.ToolPolicy` to have the runner apply approval and timeout policy to tools.
- Use `RunConfig.ToolAccessLevel`, tool fields, or `agentsdk.WrapWithPolicy` when you need more direct control.

Runnable source: [policy_test.go](policy_test.go).
