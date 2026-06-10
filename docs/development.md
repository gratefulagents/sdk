# Development

Guide for contributors and maintainers: how to build, test, and ship changes to the Grateful Agents SDK.

Run the main checks:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./...
GRATEFUL_LIVE_TESTS=skip go test -race ./...
go vet ./...
make verify-sdk-purity
GOOS=windows go test -exec /usr/bin/true ./...
GOOS=linux go test -exec /usr/bin/true ./...
```

After dependency edits:

```sh
go mod tidy
```

## Running tests

The `pkg/...` and `internal/...` test suites are self-contained — no
credentials required:

```sh
go test -race ./pkg/... ./internal/...
```

Several example suites under `examples/features/...` are **live integration
tests against real provider APIs**. They skip when credentials are missing.
Set `GRATEFUL_LIVE_TESTS=skip` to skip all live provider calls, or
`GRATEFUL_LIVE_TESTS=required` when a live run should fail fast on missing
credentials:

```sh
GRATEFUL_LIVE_TESTS=skip go test ./examples/features/...
```

### Switching providers for the whole feature suite

Every live-converted feature test goes through a single dispatch helper
(`examples/features/internal/liverunner`). You can re-run the entire
feature suite against a different provider — without touching test code —
by setting `GRATEFUL_LIVE_PROVIDER`:

```sh
# Default: live OpenAI via Codex OAuth ($HOME/.codex/auth.json)
go test ./examples/features/...

# Same suite, but every test runs against Anthropic
GRATEFUL_LIVE_PROVIDER=anthropic   ANTHROPIC_API_KEY=sk-ant-...   go test ./examples/features/...

# Same suite, but every test runs against OpenRouter
GRATEFUL_LIVE_PROVIDER=openrouter  OPENROUTER_API_KEY=sk-or-...   go test ./examples/features/...

# Same suite, dispatched through agentsdk.MultiProvider with whichever
# providers have credentials in scope. Default prefix is "openai"; switch
# with GRATEFUL_LIVE_DEFAULT_PROVIDER=anthropic|openrouter.
GRATEFUL_LIVE_PROVIDER=multi  ANTHROPIC_API_KEY=sk-ant-...  go test ./examples/features/...
```

Notes:

- `GRATEFUL_LIVE_PROVIDER=multi` builds a real `agentsdk.MultiProvider`,
  registers every provider whose credentials are present, and runs the
  default-prefix model (e.g. `openai/gpt-5.5`). This is the same
  `MultiProvider` callers use in production — the test suite is just a
  client of it.
- A handful of provider tests in `examples/features/providers/` pin
  themselves to a specific provider on purpose (the multi-provider
  routing test, the anthropic-specific test, etc.); those are unaffected
  by `GRATEFUL_LIVE_PROVIDER`.
- Some features may not pass against every provider — e.g. tests that
  rely on OpenAI-style reasoning effort, or on Anthropic-style tool
  result shapes. When that happens, the test failure is the signal. The
  feature inventory below documents which guarantees are SDK-level vs.
  provider-specific.
- Anthropic OAuth is supported by the provider runtime when the host supplies
  a refreshed access token (`ProviderConfig{AuthMode: "oauth"}`), but the live
  Anthropic example suite still uses `ANTHROPIC_API_KEY` because token
  acquisition and refresh are host responsibilities.

### Per-suite env requirements

Each suite needs different credentials for live calls. The table below maps
every test file under `examples/features/` to the env it uses under the
**default** `GRATEFUL_LIVE_PROVIDER=openai` setting.

| Suite | Test file(s) | Env that enables live calls | Optional env (overrides) |
| --- | --- | --- | --- |
| `agent_runtime` | `runtime_test.go` | OpenAI OAuth (`$HOME/.codex/auth.json`) | `OPENAI_OAUTH_AUTH_JSON_PATH`, `OPENAI_BASE_URL`, `OPENAI_LIVE_MODEL` |
| `chatloop` | `chatloop_test.go` | OpenAI OAuth | same as above |
| `context_compaction` | `context_compaction_test.go` | none (offline) | — |
| `costs` | `costs_test.go` | OpenAI OAuth | same as above |
| `errors_retries` | `errors_retries_test.go` | none (uses scripted fake — live API can't deterministically simulate retries) | — |
| `guardrails` | `guardrails_test.go` | OpenAI OAuth (first two tests) | same as above |
| `handoffs_subagents` | `handoffs_subagents_test.go` | OpenAI OAuth | same as above |
| `mcp` | `mcp_test.go` | none (offline) | — |
| `memory` | `memory_test.go` | none (offline) | — |
| `model_abstraction` | `model_abstraction_test.go` | OpenAI OAuth | same as above |
| `observability` | `observability_test.go` | OpenAI OAuth | same as above |
| `policy` | `policy_test.go` | OpenAI OAuth (`TestRunnerToolPolicyExample` only; `TestPolicyExample` is offline) | same as above |
| `providers` | `providers_test.go` | none (offline configuration test) | — |
| `providers` | `anthropic_live_test.go` | `ANTHROPIC_API_KEY` | `ANTHROPIC_BASE_URL`, `ANTHROPIC_LIVE_MODEL` (default `claude-sonnet-4-6`) |
| `providers` | `openrouter_live_test.go` | `OPENROUTER_API_KEY` | `OPENROUTER_BASE_URL`, `OPENROUTER_LIVE_MODEL` (default `deepseek/deepseek-v4-flash`) |
| `providers` | `multi_provider_live_test.go` | OpenAI OAuth **and** `ANTHROPIC_API_KEY` | OpenAI + Anthropic optional vars above |
| `sandbox` | `sandbox_test.go` | none (offline) | — |
| `settings_routing` | `settings_routing_test.go` | none (offline) | — |
| `streaming` | `streaming_test.go` | OpenAI OAuth (3 tests); 4th test (`TestSDKOpenAIStreamingViaHTTPTest`) is offline against `httptest.Server` | same as above |
| `structured_output` | `structured_output_test.go` | OpenAI OAuth | same as above |
| `tools` | `tools_test.go` | OpenAI OAuth (`TestFunctionToolExample`, `TestToolApprovalAndPolicyWrapperExample`); `TestToolRegistryExample` is offline | same as above |
| `tools_registry` | `tools_registry_test.go` | none (offline) | — |
| `tracestore` | `tracestore_test.go` | none (offline) | — |

When `GRATEFUL_LIVE_PROVIDER` is set to `anthropic` or `openrouter`, the
"OpenAI OAuth" rows above are satisfied by the corresponding provider
env (`ANTHROPIC_API_KEY` / `OPENROUTER_API_KEY`) instead.

### Recipes

```sh
# Everything that doesn't need network/credentials
GRATEFUL_LIVE_TESTS=skip go test ./...

# All OpenAI-OAuth-backed example tests (uses $HOME/.codex/auth.json)
go test ./examples/features/...

# Live Anthropic tests only
ANTHROPIC_API_KEY=sk-ant-...     go test -run TestAnthropicLive  ./examples/features/providers/

# Live OpenRouter tests only
OPENROUTER_API_KEY=sk-or-...     go test -run TestOpenRouterLive ./examples/features/providers/

# Live multi-provider routing (needs both)
ANTHROPIC_API_KEY=sk-ant-...     go test -run TestMultiProvider  ./examples/features/providers/

# Override OpenAI model used by every OAuth-backed test
OPENAI_LIVE_MODEL=gpt-5  go test ./examples/features/agent_runtime/

# Skip every live test in the entire tree
GRATEFUL_LIVE_TESTS=skip go test ./...

# Fail if selected live tests cannot find credentials
GRATEFUL_LIVE_TESTS=required go test ./examples/features/providers/
```

Configuration env contracts are defined in:

- `examples/features/internal/liverunner/liverunner.go` (provider dispatch)
- `examples/features/internal/liveopenai/liveopenai.go` (OpenAI OAuth)
- `examples/features/internal/liveanthropic/liveanthropic.go` (Anthropic)
- `examples/features/internal/liveopenrouter/liveopenrouter.go` (OpenRouter)

### Running the live suites in CI

Each provider has a manually-dispatched GitHub Actions workflow under
`.github/workflows/`:

| Workflow | Trigger | Required repo secrets | Purpose |
| --- | --- | --- | --- |
| `live-openai.yml` | `workflow_dispatch` | `CODEX_AUTH_JSON` (full contents of `~/.codex/auth.json`); optional `OPENAI_OAUTH_ACCOUNT_ID` | Full feature suite against live OpenAI via Codex OAuth |
| `live-anthropic.yml` | `workflow_dispatch` | `ANTHROPIC_API_KEY` | Full feature suite against live Anthropic |
| `live-openrouter.yml` | `workflow_dispatch` | `OPENROUTER_API_KEY` | Full feature suite against live OpenRouter |
| `live-multi.yml` | `workflow_dispatch` | any subset of the above | Full feature suite through `agentsdk.MultiProvider`; selectable default prefix |
| `terminal-bench.yml` | `workflow_dispatch` | provider-specific (same as above) | Runs Terminal-Bench against `grateful-agent-run` via the adapter at `eval/terminal_bench/grateful_agent.py` on `ubuntu-latest`. |

Each workflow accepts optional inputs (`model`, `packages`, `run`) so a
maintainer can target a single suite or test from the Actions UI without
editing the workflow.

### Running Terminal-Bench locally

For Linux hosts with Docker, git, and Python 3.12+ available, the one-command
path is:

```sh
OPENAI_OAUTH_AUTH_JSON_PATH=$HOME/.codex/auth.json ./scripts/run-terminal-bench.sh
```

The script checks Docker/git, creates a local Python venv under
`.grateful-evals/`, installs `terminal-bench` when needed, builds the
`grateful-agent-run` harness for Linux, and runs
`terminal-bench/terminal-bench-2-1` through Harbor and
`eval/terminal_bench/harbor_grateful_agent.py`. OpenAI runs use OAuth by
default. The script accepts extra `harbor run` flags after the script name,
streams debug logs to `jobs/<run-id>.harbor-run.log`, and supports these common
overrides. Terminal-Bench defaults disable SDK `WebFetch` and enable the
Terminal-Bench compliance guardrail; keep those defaults for leaderboard-intended
runs. Custom `RUN_ID` values are normalized to Docker Compose-safe lowercase
names.

```sh
GRATEFUL_PROVIDER=anthropic ANTHROPIC_API_KEY=... ./scripts/run-terminal-bench.sh
GRATEFUL_PROVIDER=openrouter OPENROUTER_API_KEY=... ./scripts/run-terminal-bench.sh
OPENAI_AUTH_MODE=api-key OPENAI_API_KEY=... ./scripts/run-terminal-bench.sh
TB_TASK_IDS=task_one,task_two TB_N_CONCURRENT=2 ./scripts/run-terminal-bench.sh
TB_DATASET=terminal-bench/terminal-bench-2-1 RUN_ID=my-run ./scripts/run-terminal-bench.sh
TB_PROFILE=leaderboard TB_N_CONCURRENT=32 ./scripts/run-terminal-bench.sh
./scripts/audit-terminal-bench-compliance.py jobs/<run-id>
```
