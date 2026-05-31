# OpenAI OAuth SDK Integration Tests

This package is the SDK-wide live integration suite. It uses OpenAI OAuth only, runs `gpt-5.5`, and skips when OAuth credentials are not available or `GRATEFUL_LIVE_TESTS=skip` is set. It does not use API-key auth and does not provide credential-free live fallbacks.

Run it:

```sh
OPENAI_OAUTH_AUTH_JSON_PATH=$HOME/.codex/auth.json go test ./test/integration/openai_oauth -count=1 -v
```

If `OPENAI_OAUTH_AUTH_JSON_PATH` is unset, the tests look for `$HOME/.codex/auth.json`. The default base URL is `https://chatgpt.com/backend-api/codex`; override it with `OPENAI_BASE_URL` only when you intentionally want a different OAuth-compatible backend.

## Coverage Matrix

| Feature | Primary entry points | Correctness and security coverage |
| --- | --- | --- |
| Agent runtime | `agentsdk.Agent`, `Runner`, `RunConfig`, `RunResult` | Live turn loop, tool-call lifecycle, final output, usage/raw responses, max turns, last-agent state, and result item shape. |
| OpenAI OAuth provider | `openai.ProviderConfig`, `AuthModeOAuth`, OAuth auth sessions | Builds providers from auth JSON, runs `gpt-5.5`, streams responses, estimates cost, and uses provider-native Responses compaction. |
| Model abstraction | `Model`, `ModelProvider`, `ModelRequest`, `ModelResponse`, `ModelStream` | Exercises both runner-level and low-level model calls with streaming and response item assertions. |
| Structured output | `OutputSchema`, `NewOutputSchema` | Sends strict JSON schema to the live model and verifies parsed typed Go output. |
| Function tools | `Tool`, `FunctionTool`, `WrapWithPolicy` | Verifies model-visible tool schemas, execution, tool output items, and approval-gated interruption without executing protected mutation. |
| Guardrails | Input, output, tool-input, and tool-output guardrails; `guardrails` package | Confirms all guardrail result channels, custom rule compilation, destructive command blocking, and secret-output tripwires. |
| Streaming | `Runner.RunStreamed`, `StreamedRunResult.Err`, `Model.StreamResponse` | Asserts streamed run-item events, terminal error surface, and low-level model-stream completion/items. |
| Runtime builder | `runtime.NewBuilder` | Builds a runnable OAuth bundle from config and verifies the resulting agent/runner can complete a live run. |
| Chat loop | `ChatLoop`, `SessionStore`, working state | Loads host messages, persists run items, and verifies conversation/working-state helper logic. |
| Handoffs | `NewHandoff`, `Agent.Handoffs` | Forces a live triage agent to transfer to a target agent and verifies target output and last-agent state. |
| Sub-agents | `Agent.AsTool`, `NewSubAgentScheduler`, `BuildSubAgentTaskTools` | Exercises synchronous sub-agent-as-tool and async spawn/wait/activity/collect task tools against live `gpt-5.5` workers. |
| Modes and routing | `mode.TemplateSpec`, routing helpers, phase gates | Verifies phase tool access, role routing, completion/reset decisions, RBAC denial, plan gates, and git-clean gates. |
| Specialists | `RoleCatalog`, `BuildSpecialistsFromCatalog` | Converts role specs into agent specialists with expected names/access metadata. |
| Policy | `RuntimePolicy`, `ToolPolicy`, `PermissionMode` | Confirms default policy normalization, read-only filtering, shell command blocking, and approval-gated mutation. |
| Built-in tools | `tools.NewRegistry` and file/search/shell/LSP/web/browser/vision/signal/memory tools | Verifies registry membership and direct correctness for write/edit/read/glob/grep/list/bash/memory/signal/vision tools. |
| Tool security | Workspace path resolution, read-only registry, web/browser URL validation, shell blockers | Rejects workspace escape writes, read-only write exposure, loopback/private URL fetches, and blocked shell commands. |
| MCP | `mcp.BuildTools`, break-glass helpers | Wraps an MCP manager as SDK tools, verifies argument forwarding/result conversion, and checks break-glass prompt content. |
| Sandbox | `sandbox.Default`, `sandbox.Request` | Runs a bounded read-only shell command through the sandbox facade and checks output/exit code. |
| Memory | `memory.Store`, `NoopEmbedder`, `tools/memory.Tool` | Stores/searches/deletes memories through both store API and tool API; verifies deterministic no-op embeddings. |
| Events and observability | `EventStream`, `SessionEventStream`, `events.LineWriter`, `RunHooks`, `TracingProcessor` | Asserts run hooks, typed host events, tool events, text events, trace starts, and span starts. |
| Trace store | `tracestore.FilesystemTraceStore` | Creates run directories, appends traces, writes scores, and filters listed runs. |
| Context compaction | `MaybeCompactRunItems`, `ContextCompactor` | Verifies deterministic SDK compaction and live provider-native Responses compaction. |
| Errors and retries | `RetryPolicy`, provider retry advice surfaces | Validates retry backoff logic and keeps retry surfaces covered by live provider calls. |
| Usage and costs | `Usage`, OpenAI/Anthropic cost estimators | Checks live usage aggregation and positive known cost estimates for known provider models. |
| Autonomous-loop helpers | `AutoTracker`, `BuildSmartNudge`, circuit breakers | Verifies no-tool stall detection and smart-nudge construction. |
| Signal and workflow tools | `AskUserQuestion`, `present_plan`, `save_plan`, `get_plan`, `finish`, `set_phase` | Verifies structured pause/control-flow tool results, durable plan storage, phase sink updates, and pause semantics. |

## Intent

These tests are intentionally broader and slower than unit tests. They protect the public SDK contracts that only show up when the runtime, OAuth provider, live model, tools, guardrails, policy, host adapters, and observability surfaces are wired together. Package-level unit tests remain responsible for exhaustive branch and edge-case coverage.
