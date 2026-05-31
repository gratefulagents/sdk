# SDK Feature Examples

These examples show focused usage of individual SDK features. They are intentionally smaller than the SDK-wide integration suite. Some packages are deterministic and offline; many packages run live provider calls through `examples/features/internal/liverunner` when credentials are present, so the same examples can exercise real provider adapters.

For the SDK-wide live OpenAI OAuth integration suite using `gpt-5.5`, run:

```sh
OPENAI_OAUTH_AUTH_JSON_PATH=$HOME/.codex/auth.json go test ./test/integration/openai_oauth -count=1 -v
```

Run every feature example:

```sh
go test ./examples/features/...
```

Missing live credentials skip by default. Set `GRATEFUL_LIVE_TESTS=required`
when a live example run should fail instead.

Run only the offline-safe pass:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/features/...
```

Run a single feature:

```sh
go test ./examples/features/tools
go test ./examples/features/tools_registry
go test ./examples/features/guardrails
```

## Feature Map

| Feature | README | Runnable package |
| --- | --- | --- |
| Agent runtime | [agent_runtime](agent_runtime/README.md) | `go test ./examples/features/agent_runtime` |
| Model abstraction | [model_abstraction](model_abstraction/README.md) | `go test ./examples/features/model_abstraction` |
| Providers | [providers](providers/README.md) | `go test ./examples/features/providers` |
| Tools | [tools](tools/README.md) | `go test ./examples/features/tools` |
| Tool registry | [tools_registry](tools_registry/README.md) | `go test ./examples/features/tools_registry` |
| MCP | [mcp](mcp/README.md) | `go test ./examples/features/mcp` |
| Sandbox | [sandbox](sandbox/README.md) | `go test ./examples/features/sandbox` |
| ChatLoop | [chatloop](chatloop/README.md) | `go test ./examples/features/chatloop` |
| Handoffs and sub-agents | [handoffs_subagents](handoffs_subagents/README.md) | `go test ./examples/features/handoffs_subagents` |
| Guardrails | [guardrails](guardrails/README.md) | `go test ./examples/features/guardrails` |
| Structured output | [structured_output](structured_output/README.md) | `go test ./examples/features/structured_output` |
| Streaming | [streaming](streaming/README.md) | `go test ./examples/features/streaming` |
| Context compaction | [context_compaction](context_compaction/README.md) | `go test ./examples/features/context_compaction` |
| Settings and routing | [settings_routing](settings_routing/README.md) | `go test ./examples/features/settings_routing` |
| Observability | [observability](observability/README.md) | `go test ./examples/features/observability` |
| Errors and retries | [errors_retries](errors_retries/README.md) | `go test ./examples/features/errors_retries` |
| Costs | [costs](costs/README.md) | `go test ./examples/features/costs` |
| Policy primitives | [policy](policy/README.md) | `go test ./examples/features/policy` |
| Memory | [memory](memory/README.md) | `go test ./examples/features/memory` |
| Trace store | [tracestore](tracestore/README.md) | `go test ./examples/features/tracestore` |

The helper packages under `internal/live*` and `internal/liverunner` are test dispatch helpers only. Application code should provide its own `agentsdk.Model`, use the provider packages directly, or assemble providers through `agentsdk.MultiProvider`.

## Feature Coverage Summary

The SDK is organized around a small runtime core plus host adapters. The core features are the agent runner, model/provider abstraction, tool execution, guardrails, structured output, streaming, handoffs, sub-agents, compaction, retries, usage, costs, hooks, traces, and event streams. Host-oriented packages add runtime bundle construction, file-backed mode/role config, policy and permission mapping, MCP integration, sandboxed command execution, memory stores, conversation helpers, mode/phase helpers, built-in tools, and trace persistence.

Use these folders when you want a short example for one feature. Use [../../test/integration/openai_oauth](../../test/integration/openai_oauth/README.md) when you want the SDK-wide live OAuth integration test.
