# Grateful Agents SDK

A Go toolkit for embedding tool-using AI agents in your application. The runner
is small, provider-neutral, and host-driven: your code stays in charge of
credentials, persistence, UI, approvals, policy, and deployment.

Builds and tests with Go 1.26.3.

## Install

```sh
go get github.com/gratefulagents/sdk
```

## Hello World

```go
package main

import (
	"context"
	"fmt"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
	sdkopenai "github.com/gratefulagents/sdk/pkg/agentsdk/providers/openai"
)

func main() {
	provider := sdkopenai.NewProviderWithConfig(sdkopenai.ProviderConfig{
		APIKey: "<your-api-key>",
	})
	runner := agentsdk.NewRunnerWithProvider(provider)

	result, err := runner.Run(context.Background(), &agentsdk.Agent{
		Name:         "hello",
		Model:        sdkopenai.DefaultChatModel,
		Instructions: "Reply with exactly: hello world",
	}, []agentsdk.RunItem{{
		Type:    agentsdk.RunItemMessage,
		Message: &agentsdk.MessageOutput{Text: "Say hello world."},
	}}, agentsdk.RunConfig{MaxTurns: 1})
	if err != nil {
		panic(err)
	}

	fmt.Println(result.FinalText())
}
```

## Features

Each feature links to a focused doc with a runnable example.

| Feature | What it does |
| --- | --- |
| [Agent runtime](examples/features/agent_runtime/README.md) | Multi-turn loop with tools, handoffs, approvals, retries, usage, and hooks. |
| [Model abstraction](examples/features/model_abstraction/README.md) | Custom `Model`/`ModelProvider`, `MultiProvider`, and provider-prefixed routing. |
| [Providers](examples/features/providers/README.md) | OpenAI (Responses/Chat), OpenAI OAuth, Anthropic, OpenRouter, local gateways. |
| [Tools](examples/features/tools/README.md) | `FunctionTool`, JSON schemas, approvals, and tool policies. |
| [Tool registry](examples/features/tools_registry/README.md) | Permission-aware built-in tools: shell, fs, search, git, LSP, web, and more. |
| [MCP](examples/features/mcp/README.md) | Load `.mcp.json` stdio servers and expose their tools and resources. |
| [Sandbox](examples/features/sandbox/README.md) | Subprocess execution with a configurable, fail-closed boundary. |
| [ChatLoop](examples/features/chatloop/README.md) | Conversation helper for interactive multi-turn sessions. |
| [Handoffs & sub-agents](examples/features/handoffs_subagents/README.md) | Agent handoffs, `Agent.AsTool`, specialists, and async sub-agent tasks. |
| [Guardrails](examples/features/guardrails/README.md) | Input/output/tool guardrails with destructive-command and secret rules. |
| [Structured output](examples/features/structured_output/README.md) | JSON-schema prompting, validation, and custom parsing. |
| [Streaming](examples/features/streaming/README.md) | Raw model deltas, run-item events, and final streamed result. |
| [Context compaction](examples/features/context_compaction/README.md) | Local and provider-native history compaction with carry-forward guards. |
| [Settings & routing](examples/features/settings_routing/README.md) | Reasoning/verbosity settings, mode routing, and role overrides. |
| [Observability](examples/features/observability/README.md) | Run/agent hooks, progress tracking, event streams, and request snapshots. |
| [Errors & retries](examples/features/errors_retries/README.md) | Typed run errors, retry policy, and capped retry-after delays. |
| [Costs](examples/features/costs/README.md) | Usage mapping and per-provider cost estimation. |
| [Policy](examples/features/policy/README.md) | Permission and access-clamping primitives. |
| [Memory](examples/features/memory/README.md) | Optional memory stores and embedders. |
| [Trace store](examples/features/tracestore/README.md) | Filesystem trace persistence and OpenTelemetry bridging. |

Run every feature example offline:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/features/...
```

## Documentation

- [Architecture](docs/architecture.md) — package boundaries and runtime flow.
- [Security model](docs/security.md) — threat model and shipped defenses. Read before exposing tools to untrusted input.
- [Development](docs/development.md) — local workflow and the live-test matrix.
- [Feature inventory](examples/features/README.md) — full list of feature examples.
- [CLI harness](cmd/grateful-agent-run) — evaluation, CI, and benchmark runs.

## Testing

```sh
GRATEFUL_LIVE_TESTS=skip go test ./...   # deterministic, offline
go vet ./...
make verify-sdk-purity
```

Live example tests skip when credentials are missing. Set
`GRATEFUL_LIVE_TESTS=required` to fail instead.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Report security issues via
[SECURITY.md](SECURITY.md), not public issues.

## License

GPL-3.0-only. See [LICENSE](LICENSE).
