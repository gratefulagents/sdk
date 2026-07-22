# Competitive analysis: Grateful Agents SDK vs. leading agent SDKs

**Snapshot date:** 2026-07-22

**Scope:** Grateful Agents SDK, OpenAI Agents SDK, LangChain/LangGraph, Google ADK, PydanticAI, Microsoft Agent Framework, CrewAI, Vercel AI SDK, and Mastra.

## Executive summary

Grateful Agents SDK is not merely another minimal agent loop. Its strongest, most defensible capabilities are:

- a provider-neutral Go runtime with native OpenAI and Anthropic implementations plus OpenAI-compatible routing;
- unusually deep controls for tool execution: access levels, approval, sandboxing, SSRF protection, secret redaction, untrusted-output tagging, and optional per-run authorization records returned to the host;
- built-in coding and operations tools rather than only generic function calling;
- handoffs, agents-as-tools, and managed background sub-agents with dependency DAGs, steering, cancellation, and bounded delegation;
- prompt-cache-aware execution, context compaction, model fallback, cost accounting, hooks, filesystem traces, and OpenTelemetry;
- durable project tasks and typed long-term memory via filesystem and SQLite stores.

Those strengths support a differentiated target position:

> **The SDK should become a governed, provider-neutral, durably resumable runtime for production Go services and autonomous developer/operations agents.**

Today it is a governed, provider-neutral Go agent runtime with persistent project state and in-process approval continuation; cross-process run resumption is roadmap work, not a current claim.

The SDK should not try to become a Go clone of the entire LangChain integration catalog or a role-playing multi-agent framework. It should win on operational correctness, governed effects, portability, and deployability.

The most important gaps are:

1. **No first-class durable run/checkpoint contract.** Conversation persistence and project state exist, but an exact run—including pauses and active child work—cannot be safely serialized, restarted, and resumed across processes.
2. **No deterministic workflow layer.** The sub-agent DAG is useful but is model-directed task delegation, not a typed, checkpointed graph with replay, retries, timers, fan-out/fan-in, compensation, and idempotent effects.
3. **Public API and typed-tool ergonomics lag Python/TypeScript leaders.** Callers manually supply raw JSON Schema and `json.RawMessage`; the top-level package exports a very large alias surface, and the runtime builder has a very large flat configuration.
4. **MCP is client-side and stdio-only.** Leaders support Streamable HTTP/SSE, OAuth, remote production servers, and often MCP server exposure.
5. **Evaluations are engineering fixtures rather than a productized Go eval SDK.** Terminal-Bench and security corpora are valuable, but the checked-in Promptfoo suite is only two smoke cases and there is no dataset/grader/experiment API.
6. **Provider breadth is partly compatibility rather than native capability parity.** Gemini, Groq, xAI, OpenRouter, Copilot, and local gateways are supported, but only OpenAI and Anthropic have deep native implementations. Adapter-specific probes exist, but there is no unified public capability profile, requirements preflight, or conformance report.
7. **No standard remote runtime, deployment contract, or browser event protocol.** Hosts can build these, but competitors substantially reduce time to production.
8. **GPL-3.0-only and pre-1.0 API maturity create potential adoption friction, especially for closed-source redistribution.** Nearly every major open competitor reviewed uses MIT or Apache-2.0. No public adoption signal was identified in the sources reviewed as of the snapshot date, and the project has reached `v0.0.88` without a published compatibility policy or changelog.

### Bottom-line recommendation

Prioritize a **durable execution kernel**, **typed stable API**, **remote MCP**, **provider conformance**, and **Go-native evals** before adding more autonomous-agent features. In parallel, make an explicit licensing and positioning decision. Only after the durable kernel is sound should the project add a general workflow graph or remote control plane.

---

## Method and caveats

This comparison used:

- the SDK source, public API, tests, examples, architecture/security/development docs, workflows, eval assets, tags, and recent commit history;
- current first-party documentation and repositories for each competitor;
- GitHub repository metadata captured on the snapshot date for directional adoption and license signals.

The capability map separates open SDK/runtime features from first-party hosted/commercial additions and uses presence markers rather than ordinal scores. Language-specific or host-owned behavior is marked as limited, and external runtimes/adapters are not credited as core capability. Google ADK and Microsoft Agent Framework still require explicit footnotes because their documented ecosystem features are not uniform across languages.

The map is a comparative judgment, not a benchmark result. GitHub stars indicate visibility, not reliability. Hosted product claims were not independently load-tested or security-audited. Most linked documentation is a moving `main`/current surface rather than an immutable snapshot; pin package versions and run compatibility tests before procurement or implementation. Release versions found during research included OpenAI Agents Python v0.18.3, OpenAI Agents TypeScript v0.13.5, CrewAI v1.15.5, and Mastra core v1.51.0, but not every dynamic documentation page is tied to those tags.

---

## Current Grateful Agents SDK inventory

### What is genuinely strong

| Area | Current capability | Evidence |
| --- | --- | --- |
| Agent runtime | Multi-turn loop, streamed and non-streamed execution, tools, handoffs, approvals, retries, model fallback, guardrails, typed run errors, usage, cost, and hooks | `README.md`; `internal/agent/runner.go`; `internal/agent/run_config.go` |
| Composition | Handoffs, agents-as-tools, synchronous/background sub-agents, keyed dependency DAGs, result forwarding, wait, steering, cancellation, concurrency limits, and one-level delegation | `docs/architecture.md`; `pkg/agentsdk/subagent_tools.go`; `internal/agent/subagent_registry.go` |
| Local governance controls | Permission modes, policy wrappers, optional action authorization records, subprocess sandbox, SSRF protection, destructive-command rules, secret detection/redaction, prompt-injection boundaries, MCP descriptor sanitization, bounded tool output, and fail-closed defaults | `docs/security.md`; `pkg/agentsdk/guardrails`; `pkg/agentsdk/sandbox`; `pkg/agentsdk/tools/web`; `eval/audit-fixtures` |
| Providers | Native OpenAI Responses/Chat and Anthropic; OpenRouter, Gemini, Groq, xAI, Copilot, local/OpenAI-compatible gateways; named routes, multi-provider routing, OAuth/API-key support, fallback models | `pkg/agentsdk/providers/factory.go`; `pkg/agentsdk/providers/openai`; `pkg/agentsdk/providers/anthropic` |
| Context management | Prompt-cache-stable run shape, explicit cache identity, deterministic/LLM and provider-native compaction, model-specific context thresholds, bounded/spilled tool results | `docs/architecture.md`; `internal/agent/runner.go`; `internal/agent/compaction_*.go` |
| Host integration | Narrow interfaces for conversation storage, dynamic configuration, status, tracing, approvals, and platform-specific tools | `pkg/agentsdk/chatloop.go`; `pkg/agentsdk/host` |
| Project state | Event-sourced tasks, dependencies, claims, comments, typed memories, session summaries, priming, lexical/hybrid retrieval, filesystem and SQLite implementations | `pkg/agentsdk/projectstate`; `docs/projectstate-tools.md` |
| MCP | Local stdio MCP client, tools/resources, configuration snapshots, reconnect handling, allowlists, credential filtering, sandboxing, break-glass approval, and sanitized operator display | `pkg/agentsdk/mcp` |
| Observability | Hooks, progress snapshots, event streams, detailed run spans, privacy-aware filesystem traces, OpenTelemetry bridge, usage and cost | `examples/features/observability`; `pkg/agentsdk/tracestore`; `pkg/agentsdk/otel` |
| Tests and benchmarks | Broad unit and integration coverage, offline CI, manually dispatched live provider suites, Terminal-Bench adapter, Promptfoo smoke evals, security corpora | `docs/development.md`; `.github/workflows`; `eval` |

### Important boundary distinctions

Several capabilities can appear more complete in a feature list than they are operationally:

- `projectstate.Store` persists **project tasks, memories, and summaries**. It is not an execution checkpoint store.
- `ChatLoop.SessionStore` persists messages and working state. It does not transactionally persist the complete runner, child scheduler, approval, and side-effect state.
- `RunResult.ToState()` returns `RunState`, but that state contains an `*Agent` with functions and runtime references and has no versioned serialization contract. It is not equivalent to OpenAI's serializable paused `RunState` or LangGraph's checkpointer.
- The sub-agent scheduler can serialize task records, but work that was active at restart is restored as failed tombstones rather than resumed (`RestoreSchedulerCheckpoint`).
- The sub-agent dependency DAG schedules specialist calls. It is not a deterministic application workflow graph.
- Provider names in the factory do not imply native feature parity. Several are routed through the OpenAI-compatible implementation.
- Trace persistence and OTel export are foundations, not a searchable trace/evaluation product.

The governance controls are broad, but they are not a blanket security guarantee. The documented threat model trusts the host process; Bubblewrap enforcement is Linux-specific unless a host supplies another executor; general egress/DLP, CPU/RSS controls, filesystem confidentiality, and co-tenant side channels are outside the shipped boundary; and danger-full-access can execute with host privileges. See `docs/security.md` before interpreting any comparison below.

---

## Comparative capability map

This map avoids ordinal rankings. It records whether a capability is first-class in the open SDK/runtime (`✓`), limited, language-specific, or host-owned (`△`), mainly supplied by an external adapter/runtime (`○`), or not found as a first-class feature (`—`). It compares documented capability presence, not implementation quality, production assurance, or feature parity across every language/provider.

| Core/open SDK capability | Grateful | OpenAI Agents | LangGraph / LangChain | Google ADK ecosystem¹ | PydanticAI | Microsoft AF ecosystem² | CrewAI | Vercel AI SDK | Mastra |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Managed agent loop | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Deterministic workflow API | — | — | ✓ | ✓ | △ | ✓ | ✓ | △ | ✓ |
| First-class durable run checkpoints | — | △ | ✓ | △ | ○ | ✓ | △ | — | ✓ |
| Cross-process serialized pause/resume | — | ✓ | ✓ | △ | △³ | ✓ | △ | — | ✓ |
| Handoffs/sub-agents/multi-agent composition | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | △ | ✓ |
| Generated typed tool/output schemas | △ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Multiple model-provider families | ✓⁴ | ✓⁵ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Unified public capability profile and preflight | —⁶ | △ | △ | △ | ✓ | △ | △ | ✓ | ✓ |
| Built-in local governance controls | ✓⁷ | ✓ | △ | △ | ✓ | ✓ | △ | △ | △ |
| Built-in coding/operations toolset | ✓ | ✓ | △ | △ | △ | ✓ | △ | ✓ | △ |
| MCP client beyond stdio | — | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| MCP server exposure | — | △ | △ | ✓ | ✓ | △ | — | — | ✓ |
| Session store abstraction | ✓ | ✓ | ✓ | ✓ | △ | ✓ | ✓ | △ | ✓ |
| Cross-session/long-term memory abstraction | ✓ | △ | ✓ | ✓ | △ | ✓ | ✓ | △ | ✓ |
| OpenTelemetry/open trace export | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Reusable evaluation library | △ | △ | △⁸ | △⁹ | ✓ | △ | △ | △ | ✓ |
| Versioned application event/stream protocol | △ | ✓ | ✓ | ✓ | ✓ | ✓ | △ | ✓ | ✓ |
| A2A client/server | — | — | — | △ | — | △ | — | — | — |
| Reference remote agent server | — | — | ○⁸ | ✓ | ○ | ✓ | ○ | — | ✓ |
| Go implementation | ✓ | — | — | ✓ | — | △ | — | — | — |

Footnotes:

1. Google ADK is a multi-language ecosystem. Go 2.0 is advertised as GA, but some documented capabilities—especially evaluations—remain Python-only or have unclear Go parity.
2. Microsoft Agent Framework combines mature Python/.NET surfaces with a Go public preview that omits several documented ecosystem features.
3. PydanticAI exposes typed deferred-tool continuation; full workflow durability and cross-process resumption are primarily supplied by Temporal, DBOS, Prefect, or Restate integrations.
4. Grateful has deep native OpenAI/Anthropic implementations and several OpenAI-compatible routes; a provider name does not establish native feature parity.
5. OpenAI Agents has a portable core model abstraction, while many hosted tools, Realtime, tracing, and server-state features are OpenAI-specific.
6. Grateful has adapter-specific probes for compaction, chat compatibility, model limits, and related behavior, but no unified public requirement/preflight contract.
7. This denotes control coverage, not a security certification. The host-trusted and platform-specific boundaries above apply.
8. LangGraph/LangChain's full evaluation and remote-server products are primarily LangSmith capabilities, not the OSS core alone.
9. Google ADK's official evaluation material reviewed for this snapshot is explicitly Python-scoped.

### First-party platform and commercial additions

These capabilities are intentionally separated from the open SDK map rather than credited as if they were core-library features.

| Ecosystem | First-party hosted/commercial additions observed |
| --- | --- |
| Grateful | None; embedded SDK and CLI/evaluation harness only |
| OpenAI | Hosted tools/containers/MCP, platform tracing, evaluations, Conversations/server state, Realtime services |
| LangChain | LangSmith tracing, datasets/evaluations, prompt tooling, Agent Server, queues, cron, cloud/hybrid/self-hosted deployment tiers |
| Google ADK | Google Agent Runtime/Platform, Cloud Run/GKE paths, Cloud Trace and Google model/platform integrations |
| PydanticAI | Pydantic Logfire observability; durable execution primarily integrates external runtimes |
| Microsoft AF | Azure AI Foundry and broader Microsoft cloud integrations |
| CrewAI | Enterprise/AMP deployment, triggers, monitoring, approval webhooks, RBAC, and integration catalog |
| Vercel AI SDK | Vercel AI Gateway and surrounding Vercel application deployment ecosystem |
| Mastra | Mastra Platform and Studio, hosted deployment/observability/evaluation surfaces; some repository content has a separate EE license |

The most direct technical competitor is now **Google ADK Go**, but comparisons must account for its uneven cross-language parity. The strongest conceptual benchmark is **LangGraph** for durability; **PydanticAI/OpenAI/Vercel/Mastra** set the developer-experience bar; **Mastra and LangSmith** demonstrate the value of integrated observability/evaluation products outside the core SDK.

---

## Competitor profiles and lessons

### OpenAI Agents SDK

**Where it leads**

- exceptionally small conceptual API: agent, runner, tool, handoff, guardrail;
- typed function tools generated from Python annotations/Pydantic or TypeScript/Zod;
- a clear distinction between handoffs and agents-as-tools;
- practical session adapters, including SQLite, Redis, SQLAlchemy, MongoDB, Dapr, and OpenAI Conversations;
- serializable interrupted run state and approval/rejection resumption through nested agents;
- local MCP over stdio, SSE, and Streamable HTTP plus OpenAI-hosted MCP;
- built-in tracing with a hosted dashboard and OpenAI platform evaluations;
- Realtime voice agents and first-party hosted tools/containers.

**Weakness/opening**

- many headline capabilities are Responses API or OpenAI-platform features rather than truly portable;
- it is an agent loop, not a deterministic durable graph engine;
- its tracing/eval defaults create platform gravity;
- guardrail coverage differs by tool/composition type and can be easy to misunderstand.

**What to copy:** the small golden path, typed tools, explicit handoff semantics, and serializable HITL state.

**What not to copy:** treating vendor-hosted tools or telemetry as portable core abstractions.

### LangChain and LangGraph

**Where they lead**

- typed graph and functional workflow APIs with nodes, conditional edges, parallelism, and compilation;
- per-thread checkpointers separate from cross-thread long-term stores;
- durable execution, recovery, time travel, HITL interrupts, and rich streaming modes;
- enormous provider, retrieval, vector-store, loader, and tool ecosystem;
- LangSmith closes the loop with tracing, datasets, evaluators, experiments, online evaluation, deployment, queues, assistants, threads, runs, cron, and SSE.

**Weakness/opening**

- breadth and abstraction density create complexity and upgrade churn;
- replay after an interrupt can restart a node, making idempotency the developer's responsibility;
- the strongest production experience is tied to the commercial LangSmith control plane;
- Python and TypeScript event/API surfaces do not always evolve identically.

**What to copy:** checkpointer/store separation, durable event model, explicit workflow/agent distinction, and remote resource model.

**What not to copy:** the integration sprawl or a graph DSL requirement for every simple agent.

### Google Agent Development Kit

**Where it leads**

- the most direct Go competitor, with Go 2.0 advertised as GA;
- sequence, loop, parallel, graph, dynamic, and collaborative workflow styles;
- clear session, state, and memory services;
- MCP client and server support across multiple languages;
- experimental A2A with Go client/server examples;
- first-party deployment to Google Agent Runtime/Platform, Cloud Run, and GKE.

**Weakness/opening**

- language parity is uneven; official evaluation documentation is Python-only;
- cloud deployment and Gemini integration create Google gravity;
- documentation reviewed did not establish LangGraph-equivalent cross-language workflow replay semantics.

**What to copy:** explicit language/provider feature matrices, MCP server support, A2A interoperability, and container/cloud deployment guidance.

**What to beat:** Go evals, vendor neutrality, security governance, and recovery semantics.

### PydanticAI

**Where it leads**

- arguably the best type-centric Python API for dependencies, tool input, structured output, validation, and testing;
- broad native provider catalog with model profiles/capabilities;
- code-first Pydantic Evals with datasets, cases, deterministic and model judges, experiments, and span assertions;
- MCP client/server and UI protocol adapters;
- strong deferred-tool/HITL semantics;
- durable execution integrations with Temporal, DBOS, Prefect, and Restate rather than pretending an in-process loop is durable.

**Weakness/opening**

- Python only;
- conversation history and durable execution are intentionally caller/external-runtime concerns;
- no first-party deployment/control-plane product.

**What to copy:** type-driven schemas, provider profiles, `go test`-like eval ergonomics, and honest external durability adapters.

### Microsoft Agent Framework

**Where it leads**

- production-oriented Python/.NET framework and emerging Go implementation;
- graph workflows, checkpointing, middleware, approvals, OTel, and Azure/Foundry integrations;
- backed by migration paths from Semantic Kernel and AutoGen.

**Weakness/opening**

- Go remains public preview and omits important features such as functional workflows, RAG, CodeAct, and declarative agents;
- Azure/Microsoft ecosystem gravity;
- documentation and repository positioning can lag one another.

**What to copy:** enterprise middleware/checkpoint posture and clear migration/versioning discipline.

**What to beat:** complete, coherent Go feature parity and provider neutrality.

### CrewAI

**Where it leads**

- accessible Agent/Crew/Flow mental model;
- event-driven flows with state, branching, loops, and persist/resume claims;
- strong out-of-box multi-agent, memory, tools, and remote MCP transport support;
- significant community visibility and a commercial operations product.

**Weakness/opening**

- role-playing crews can introduce nondeterminism where ordinary workflows are clearer;
- memory scope/category inference by an LLM is convenient but unsuitable as an authorization boundary;
- strongest HITL/operations features lean toward the commercial product.

**What to copy:** separate deterministic flows from autonomous crews and offer a simple first-run experience.

**What to avoid:** making a “crew” the default abstraction or inferring security scope from model output.

### Vercel AI SDK

**Where it leads**

- best frontend integration and streaming developer experience;
- a documented, versioned SSE data-stream protocol usable by non-TypeScript backends;
- typed tools with optional execution, which cleanly supports client-side or queued dispatch;
- broad provider registry, middleware, and capability-aware model interfaces;
- Streamable HTTP/SSE/stdio MCP with OAuth;
- strong deterministic mock providers and stream testing.

**Weakness/opening**

- it is primarily an application/model/UI SDK rather than a durable execution runtime;
- tool approval requires application orchestration and another model call rather than a durable paused job;
- memory and persistence are intentionally compositional.

**What to copy:** a versioned transport-neutral event protocol, typed tool declaration separate from dispatch, mocks, and frontend adapters.

**What not to copy:** UI-framework coupling in the core Go runtime.

### Mastra

**Where it leads**

- integrated TypeScript stack: agents, typed workflows, suspend/resume, storage-backed memory, MCP client/server, observability, evals, Studio, server, and deployment adapters;
- request-specific dynamic toolsets for multi-tenant credentials;
- rich workflow control flow and visualization;
- correlated traces/logs/metrics/feedback plus offline and sampled live evaluation.

**Weakness/opening**

- broad all-in-one framework and platform create more coupling than an embeddable Go library;
- TypeScript/Node operational model is not ideal for every backend, CLI, or infrastructure agent;
- some repository content uses a separate enterprise license.

**What to copy:** clean separation between agent loops and typed workflows, MCP server exposure, correlated evaluation, and reference server/Studio concepts.

**What not to copy:** requiring the hosted platform for core runtime value.

---

## Detailed gap analysis

### 1. Durable execution is the largest technical gap

Today, durability is spread across:

- conversation items in `ChatLoop.SessionStore`;
- working state supplied by the host;
- tasks/memories/summaries in `projectstate.Store`;
- trace files;
- sub-agent scheduler checkpoints;
- `RunState`, which is not a versioned serializable continuation.

This is not enough to guarantee:

- restart after every model, tool, handoff, approval, and child-task boundary;
- safe retry or reconciliation of mutating effects after a crash;
- optimistic locking when two workers resume one run;
- migration of persisted state across SDK versions;
- persistent timers, external events, queues, or cancellation;
- continuation of active child work rather than marking it failed;
- a durable approval inbox with authenticated decisions.

**Required foundation**

Introduce separate contracts:

```go
type RunStore interface {
    Create(ctx context.Context, run RunRecord) error
    Load(ctx context.Context, runID string) (RunRecord, error)
    CompareAndSwap(ctx context.Context, runID string, version int64, next RunRecord) error
    AppendEvents(ctx context.Context, runID string, events []RunEvent) error
}

type EffectStore interface {
    Prepare(ctx context.Context, effect EffectRecord) (EffectLease, error)
    MarkDispatched(ctx context.Context, lease EffectLease) error
    Complete(ctx context.Context, lease EffectLease, result EffectResult) error
    MarkOutcomeUnknown(ctx context.Context, lease EffectLease, reason string) error
}
```

The exact API needs design work, but the invariants should be fixed first:

- versioned and JSON/Protobuf-serializable state;
- stable IDs for run, attempt, step, tool call, approval, and child run;
- compare-and-swap or transactional ownership;
- explicit effect classes: `idempotent`, `deduplicated`, and `non-replayable`;
- stable idempotency keys propagated to destinations that honor them;
- durable effect states such as prepared, dispatched, succeeded, failed, and outcome-unknown;
- no automatic retry of a non-idempotent effect whose outcome is unknown; require reconciliation or operator action;
- atomic snapshot/event/effect transitions when records share a transactional store;
- immutable event history plus snapshots;
- explicit state migrations;
- secrets referenced, not serialized into continuations;
- resume tested by forced process termination at every boundary.

No SDK can promise exactly-once execution for an arbitrary external side effect: a process can die after the destination commits but before the local completion record is written. The defensible guarantee is deduplication where the destination honors an idempotency key, plus safe `outcome-unknown` handling everywhere else.

A general graph API should build on this kernel, not precede it.

### 2. API ergonomics and stability need a deliberate redesign

Current friction:

- `FunctionTool` requires hand-authored `json.RawMessage` schema and a raw-message callback;
- `OutputSchema` requires manual schema and optional parse function;
- the top-level `aliases.go` re-exports a very large internal surface;
- `runtime.Config` is a large, flat struct that mixes provider, agent, security, tools, tracing, project state, and host concerns;
- advanced configuration is discoverable mainly by source browsing;
- there is no compatibility policy, migration guide, or changelog despite many `v0.0.x` tags.

**Recommended public layers**

1. **Golden path:** small constructors for the common case.
2. **Typed path:** generic helpers that derive deterministic JSON Schema and validate boundaries.
3. **Advanced path:** current interfaces/raw schema escape hatches.
4. **Integration packages:** providers, storage, MCP, sandbox, evals, and server adapters outside the minimal core.

Illustrative direction:

```go
tool := agentsdk.FuncTool[SearchInput, SearchOutput](
    "search",
    "Search the product catalog",
    search,
    agentsdk.ReadOnly(),
)

agent := agentsdk.NewAgent[Answer](
    "support",
    agentsdk.WithModel("anthropic/claude-sonnet"),
    agentsdk.WithTools(tool),
)
```

Do not remove raw `Tool` or `OutputSchema`; add a much better default above them. Before `v1`, reduce the alias surface and publish:

- supported Go version policy;
- semantic-versioning and deprecation policy;
- compatibility tests for persisted state and event schemas;
- changelog and migration guides;
- package stability labels (`stable`, `experimental`, `internal`).

Also reconcile the documented Go requirement (`README.md` says 1.26.3 while `go.mod` declares 1.26.2).

### 3. Provider support needs capabilities, not only names

A portable application needs to know whether a model/provider supports:

- streaming and exact event types;
- parallel tool calls;
- strict JSON Schema and structured outputs;
- tool-choice controls;
- reasoning items;
- images, files, audio, and realtime;
- hosted tools and remote MCP;
- prompt caching and provider-native compaction;
- resumable response IDs/conversations;
- usage/cache semantics and reliable cost data.

Add a provider/model profile:

```go
type ModelCapabilities struct {
    Streaming            bool
    ParallelTools        bool
    StrictJSONSchema     bool
    NativeStructuredData bool
    Vision               bool
    Audio                bool
    PromptCaching        bool
    NativeCompaction     bool
    HostedTools          map[string]bool
}
```

Then add a conformance suite that every adapter runs. An agent/run should be preflighted and either:

- execute with guaranteed requirements;
- use an explicit fallback/emulation;
- fail early with a precise unsupported-capability error.

Provider priorities should be based on target users, but likely native additions are Gemini/Vertex, AWS Bedrock, and Azure OpenAI/Foundry. OpenAI-compatible routing should remain, but it should not imply verified parity.

### 4. MCP needs remote transports, server mode, and enterprise auth

Immediate gaps:

- only stdio is accepted by `mcp.ServerConfig`;
- no Streamable HTTP or legacy SSE client;
- no OAuth discovery/refresh or per-tenant header injection for remote MCP;
- no SDK MCP server that exposes agents, tools, resources, workflows, or prompts;
- no distributed connection/session ownership model.

The existing security controls—allowlists, snapshot pinning, descriptor sanitization, sandboxing, credential filtering, and break glass—are valuable and should be preserved across remote transports.

Recommended order:

1. transport/auth interface design and an explicitly experimental, read-only Streamable HTTP client;
2. remote-specific URL/DNS/redirect/SSRF, TLS, OAuth audience/refresh, token-redaction, tenant-isolation, and lifecycle tests;
3. durable approval, effect classification, idempotency, and outcome-unknown handling before remote mutations are enabled;
4. transport-neutral connection/session manager, with SSE compatibility only where needed;
5. production remote MCP plus centralized policy, quotas, provenance, and audit hooks;
6. MCP server package;
7. remote integration and conformance tests.

### 5. Human approval should become a durable security protocol

`ApprovalGate` is useful for in-process host integration, but production approvals require:

- an immutable request hash over tool, arguments, identity, policy, and expiry;
- authenticated approver identity and authorization scope;
- allow/deny/edit decisions with reason and audit trail;
- expiry, escalation, cancellation, and optional multi-party policy;
- persistence separate from model-visible history;
- resume on another worker/process;
- exact binding between approval and the effect executed.

A client saying “approved” must never itself be the authorization boundary. The run store and effect store should enforce that the approved immutable request is the request being executed.

### 6. Evals need to become a Go library, not only scripts

Current assets demonstrate strong testing discipline, but not a reusable evaluation system.

Add a package that works naturally with `go test`:

- versioned datasets and cases;
- mock/recorded model providers;
- golden final outputs and structured-output assertions;
- tool trajectory assertions (called/not called/order/arguments/results);
- trace/span assertions;
- deterministic evaluators and model-judge interfaces;
- cost/latency/turn/tool-call budgets;
- repeated trials and statistical summaries;
- baseline comparison and CI regression thresholds;
- trace replay into new prompts/models/policies;
- export to standard JSON and optional external platforms.

The first public benchmark should include more than Terminal-Bench:

- tool selection/argument correctness;
- cross-provider conformance;
- approval and restart behavior;
- prompt-injection and MCP trust-boundary cases;
- long-context/compaction fidelity;
- multi-agent delegation efficiency;
- structured-output reliability.

Publish pinned configurations and results; otherwise benchmark integrations are difficult for users to assess.

### 7. Streaming should become a versioned public protocol

The SDK has raw model deltas, run-item events, progress, and JSONL session events, but no documented cross-language protocol comparable to Vercel's UI stream or LangGraph Agent Server SSE.

Define one semvered event envelope with:

- protocol/schema version;
- run/thread/step/attempt/parent identifiers;
- monotonically ordered sequence/cursor;
- trace context and timestamp;
- text/structured/reasoning deltas;
- model, tool, handoff, child-run, checkpoint, approval, source, error, and terminal events;
- redaction classification;
- resume cursor and backpressure rules.

Expose it as Go iterators/channels, JSONL, SSE, WebSocket, and optionally gRPC without changing event meaning. Frontend adapters can then target Vercel AI streams, AG-UI, or custom UIs without coupling the runtime to React/Node.

### 8. Deployment should have a supported reference path

“Host-owned” should remain a design principle, but it should not mean every adopter invents the same production shell.

Ship an optional reference server with:

- versioned REST/OpenAPI resources for agents, threads, runs, approvals, and events;
- synchronous and queued runs;
- SSE streaming and cancellation;
- run/checkpoint storage interfaces;
- health/readiness and graceful shutdown;
- authentication/tenant hooks, quotas, and concurrency policy;
- OTel, metrics, structured logs, and trace links;
- container image and Kubernetes examples;
- PostgreSQL reference store and one queue adapter.

Keep this outside the minimal embedded runtime. A hosted control plane can come later, if ever.

### 9. Licensing and adoption need an explicit product decision

`CONTRIBUTING.md` explicitly says GPLv3 was chosen because the project does not want agent runtimes to become a proprietary moat. GPL permits commercial use, but its reciprocal source/distribution obligations can be incompatible with closed-source redistribution policies. That creates potential tension with the stated product shape—a library embedded in host applications—especially because Go binaries are commonly statically linked. Some proprietary adopters or enterprise legal teams may therefore decline the SDK or require a separate commercial license.

Options:

1. **Apache-2.0 or MIT core:** minimizes license friction and matches the open-source terms used by most competitors reviewed; build commercial value elsewhere.
2. **Dual licensing:** GPL for open-source use plus a commercial embedding license.
3. **Keep GPL-only:** accept the potential constraint on closed-source redistribution and position explicitly for copyleft/open deployments.

This is a strategy decision, not an engineering cleanup. It should be decided before investing heavily in ecosystem integrations. If GPL remains, document common embedding/distribution implications clearly; do not leave adopters to infer them.

Other adoption fundamentals:

- create a documentation site with API reference and task-oriented guides;
- publish release notes and compatibility guarantees;
- add repository description, topics, discussions, and roadmap;
- publish benchmark results and example production architectures;
- provide issue templates and a public support/version matrix;
- stabilize a small `v1` surface rather than continuing indefinitely through `v0.0.x` tags.

### 10. Autonomous runs need cumulative execution budgets

The runtime already has important point controls: maximum turns, per-request output-token settings, model inactivity timeout, tool timeout, child turn limits, and child concurrency. Cost and token usage are recorded. It does not expose one unified cumulative run budget for total elapsed time, input/output/cache tokens, estimated spend, model/tool calls, or child spawns. Accounting is not enforcement, and child work can multiply a parent's resource use.

Add a run budget that:

- is checked before every model call, mutating effect, and child spawn;
- partitions or explicitly shares limits with child runs;
- survives checkpoint/recovery without resetting counters;
- distinguishes estimates from hard provider/account enforcement;
- emits a deterministic typed termination reason and final event;
- can reserve capacity for cleanup, finalization, or required audit writes.

This belongs in the durable state design because budget counters and reservations must be restart-safe.

---

## Prioritized roadmap

### P0: establish the product contract (0–6 weeks)

1. **Decide positioning and license.** Confirm the target is an embeddable production Go SDK; choose permissive, dual, or deliberately GPL-only licensing.
2. **Publish API/version policy.** Define supported Go versions, compatibility/deprecation rules, stable versus experimental packages, changelog, and migration format.
3. **Design durable execution and data-governance invariants.** Write an architecture decision record for run IDs, event log, snapshots, ownership/CAS, effect classes and outcome-unknown handling, approvals, child runs, migrations, tenant keys, data classification, redaction boundaries, retention/deletion, encryption/KMS hooks, and audit access.
4. **Design cumulative execution budgets.** Cover total elapsed time, input/output/cache tokens, estimated/hard cost, model/tool calls, child spawns, and child budget inheritance/partitioning. Define checks before model calls and mutating effects plus deterministic exhaustion events.
5. **Add model capability profiles and provider conformance tests.** Publish an honest matrix for every provider/model path.
6. **Prototype Streamable HTTP MCP as experimental/read-only.** Design transport-neutral lifecycle and pluggable auth/header interfaces; add URL policy, DNS/redirect/SSRF, TLS, OAuth audience/refresh, token-redaction, tenant-isolation, and connection-cleanup requirements before enabling remote mutations.
7. **Improve the golden-path API.** Prototype typed function tools and structured output without removing advanced interfaces.

**Exit criteria**

- a new user can build a typed tool agent without writing raw JSON Schema;
- an unsupported provider feature fails before the first model request;
- an experimental read-only Streamable HTTP MCP path passes remote-specific SSRF, TLS, auth-redaction, tenant-isolation, redirect/DNS, and cleanup tests;
- cumulative budget semantics and child inheritance are specified before durable state schemas are frozen;
- public compatibility and licensing choices are unambiguous.

### P1: durable, testable execution (6–16 weeks)

1. Implement versioned `RunStore`, event, snapshot, and effect-state contracts, including atomic transitions where one transactional store is used.
2. Enforce cumulative execution budgets with deterministic terminal events, recovery behavior, and child inheritance/partitioning.
3. Make tool approvals durable, identity-bound, expiring, and resumable on another process; bind each decision to the immutable effect request.
4. Make child runs first-class durable runs rather than only in-memory scheduler work.
5. Add filesystem and PostgreSQL reference run stores with tenant isolation, retention/deletion, and encryption hooks; consider Temporal/Restate adapters rather than rebuilding every scheduling feature.
6. Define the versioned event/effect protocol and SSE adapter before evaluation APIs depend on trace shapes.
7. Build `agentsdk/eval` with datasets, trajectory assertions, mock/record providers, graders, and CI baselines.
8. Graduate remote MCP to mutating/production use only after durable approval/effect handling exists; add reconnection/session cleanup and remote outcome reconciliation.
9. Add native Gemini/Vertex, Bedrock, or Azure adapters based on user demand and run them through conformance tests.

**Exit criteria**

- kill/restart tests pass after every model, tool, approval, handoff, and child-run boundary;
- recovery deduplicates effects for adapters honoring idempotency keys and never automatically replays non-idempotent effects with unknown outcomes;
- persisted state survives a minor SDK upgrade through tested migration;
- cross-tenant isolation, deletion/retention, and sensitive-data handling tests pass for reference stores;
- cumulative budget exhaustion and child-inheritance tests pass before and after recovery;
- production remote MCP passes URL/DNS/redirect/TLS, OAuth, token-redaction, tenant-isolation, reconnect, cleanup, approval, and outcome-reconciliation tests;
- a Go test can evaluate output, tool trajectory, cost, latency, and trace properties;
- all official providers publish conformance results.

### P2: deterministic workflows and reference runtime (4–8 months)

1. Build a typed workflow API on the durable kernel: steps, conditions, parallel map, fan-in, retry, timeout, wait for event, compensation, and nested agent steps.
2. Keep `Runner` as the small agent loop; do not force simple agents into the workflow API.
3. Add an optional reference server with agents/threads/runs/approvals/events, OpenAPI, SSE, cancellation, queue workers, and PostgreSQL.
4. Add MCP server exposure for tools, agents, resources, and workflows.
5. Add A2A client/server only after protocol maturity and concrete demand; begin with conformance tests and capability negotiation.
6. Add trace/eval storage and a minimal local run comparison UI, or integrate deeply with existing OTel/eval products rather than prematurely building a hosted platform.

**Exit criteria**

- workflows recover deterministically and isolate replay-safe effects;
- local embedded and remote-server execution share the same event/state semantics;
- SDK tools/agents can be exposed over MCP without bypassing policy;
- one container/Kubernetes reference deployment can be reproduced from docs.

### P3: optional expansion

- realtime audio/voice;
- browser/UI component libraries;
- hosted control plane or managed workers;
- broader provider/integration marketplace;
- visual workflow builder;
- advanced online evaluation and feedback sampling.

These should follow actual user demand. OpenAI and frontend SDKs already lead voice/UI; copying them before durability and DX are solved would dilute the Go-specific advantage.

---

## What not to do

- Do not chase LangChain's 1,000+ integrations one by one.
- Do not make multi-agent “crews” the default solution for deterministic business logic.
- Do not build a graph DSL before the execution state and side-effect contracts are durable.
- Do not claim provider parity merely because an endpoint accepts OpenAI-compatible requests.
- Do not treat conversation history or vector memory as execution durability.
- Do not make client-submitted approval an authorization decision.
- Do not couple core tracing/evals to one hosted vendor.
- Do not build a React UI into the Go core; publish a protocol and adapters.
- Do not infer tenant, memory, or authorization scope from model output.
- Do not leave long-running autonomous loops without turn, time, tool, token, cost, and child-concurrency budgets.

---

## Suggested success metrics

### Reliability

- recovery test coverage at 100% of model/tool/approval/handoff/child boundaries;
- no duplicate effects in forced-crash suites when the destination honors the SDK idempotency key;
- no automatic replay of non-idempotent effects in `outcome-unknown` state, with tested reconciliation paths;
- checkpoint migration tests across every supported minor version;
- cumulative time/token/cost/tool/child budgets enforced before and after recovery;
- cancellation and deadline propagation conformance for every provider/tool transport.

### Developer experience

- typed hello-world tool in under 30 lines and no raw schema;
- durable approval example in under 100 application lines;
- one stable import path for the common case;
- all public APIs documented and included in compatibility checks;
- time-to-first-streaming-agent under ten minutes from a clean Go environment.

### Portability

- published provider capability/conformance matrix;
- the same core eval suite passing on OpenAI, Anthropic, Gemini/Vertex, and one local gateway;
- stdio and Streamable HTTP MCP passing shared lifecycle/security tests;
- event protocol consumed by at least one non-Go frontend adapter.

### Evaluation and operations

- pinned public benchmark runs with cost, latency, success, and configuration;
- CI gates for output quality, tool trajectory, security regressions, and restart behavior;
- every run linked across logs, metrics, traces, checkpoints, approvals, and eval scores;
- documented retention, redaction, and tenant-isolation controls.

### Adoption

- explicit license and compatibility policy;
- stable `v1` roadmap and changelog;
- production reference architectures and reproducible deployment examples;
- external users/contributors and provider/storage adapters maintained outside the core team.

---

## Market and maturity snapshot

Repository metadata captured on 2026-07-22 is directional only:

| Project | Primary language | Approx. stars | License | Maturity signal |
| --- | --- | ---: | --- | --- |
| Grateful Agents SDK | Go | 0 | GPL-3.0-only | New repository; active rapid development; `v0.0.88` |
| OpenAI Agents Python | Python | 28.1k | MIT | Active releases; first-party OpenAI |
| LangGraph | Python | 37.8k | MIT | Mature OSS plus LangSmith platform |
| Google ADK Python | Python; multi-language ecosystem | 20.8k | Apache-2.0 | Go 2.0 advertised GA; Google Cloud path |
| PydanticAI | Python | 18.7k | MIT | Strong typed/eval ecosystem |
| Microsoft Agent Framework | Python/.NET; Go preview | 12.3k | MIT | Microsoft-backed successor path |
| CrewAI | Python | 56.0k | MIT | Large community plus commercial platform |
| Vercel AI SDK | TypeScript | 25.7k | Apache-2.0 | Leading frontend/model SDK ecosystem |
| Mastra | TypeScript | 26.4k | Apache-2.0 core; separate EE content | Integrated framework/platform |

Google's count is for `google/adk-python` and must not be read as Go-specific adoption. Microsoft's count is for the combined Agent Framework repository, not its preview Go implementation. License cells describe the referenced open repository/core and do not imply that every hosted or enterprise feature uses the same terms.

The SDK's test and governance depth is unusually high for its age. The risk is not a weak implementation; it is that a large pre-1.0 surface, GPL obligations for closed-source redistribution, limited durable execution, and no established public ecosystem may prevent that engineering quality from translating into adoption.

---

## Primary sources

### Grateful Agents SDK

- `README.md`
- `docs/architecture.md`
- `docs/security.md`
- `docs/development.md`
- `pkg/agentsdk/aliases.go`
- `pkg/agentsdk/chatloop.go`
- `pkg/agentsdk/runtime/builder.go`
- `pkg/agentsdk/providers/factory.go`
- `pkg/agentsdk/mcp`
- `pkg/agentsdk/projectstate`
- `pkg/agentsdk/tracestore`
- `pkg/agentsdk/otel`
- `internal/agent/run_result.go`
- `internal/agent/subagent_registry.go`
- `eval`

### OpenAI Agents SDK

- https://openai.github.io/openai-agents-python/
- https://openai.github.io/openai-agents-python/running_agents/
- https://openai.github.io/openai-agents-python/tools/
- https://openai.github.io/openai-agents-python/handoffs/
- https://openai.github.io/openai-agents-python/sessions/
- https://openai.github.io/openai-agents-python/human_in_the_loop/
- https://openai.github.io/openai-agents-python/mcp/
- https://openai.github.io/openai-agents-python/tracing/
- https://openai.github.io/openai-agents-python/models/
- https://github.com/openai/openai-agents-python
- https://openai.github.io/openai-agents-js/

### LangChain, LangGraph, and LangSmith

- https://docs.langchain.com/oss/python/langchain/overview
- https://docs.langchain.com/oss/python/langgraph/overview
- https://docs.langchain.com/oss/python/langgraph/persistence
- https://docs.langchain.com/oss/python/langgraph/interrupts
- https://docs.langchain.com/oss/python/langgraph/streaming
- https://docs.langchain.com/oss/python/langgraph/workflows-agents
- https://docs.langchain.com/oss/python/langchain/mcp
- https://docs.langchain.com/oss/python/langchain/multi-agent
- https://docs.langchain.com/langsmith/observability
- https://docs.langchain.com/langsmith/evaluation
- https://docs.langchain.com/langsmith/deployment
- https://docs.langchain.com/langsmith/agent-server
- https://github.com/langchain-ai/langgraph

### Google ADK

- https://google.github.io/adk-docs/
- https://google.github.io/adk-docs/workflows/
- https://google.github.io/adk-docs/agents/models/
- https://google.github.io/adk-docs/sessions/
- https://google.github.io/adk-docs/mcp/
- https://google.github.io/adk-docs/evaluate/
- https://google.github.io/adk-docs/a2a/
- https://google.github.io/adk-docs/deploy/
- https://github.com/google/adk-python

### PydanticAI

- https://ai.pydantic.dev/
- https://ai.pydantic.dev/models/
- https://ai.pydantic.dev/mcp/
- https://ai.pydantic.dev/multi-agent-applications/
- https://ai.pydantic.dev/tools-toolsets/deferred-tools/
- https://ai.pydantic.dev/capabilities/durable_execution/overview/
- https://ai.pydantic.dev/evals/
- https://ai.pydantic.dev/integrations/ui/overview/
- https://github.com/pydantic/pydantic-ai

### Microsoft Agent Framework

- https://learn.microsoft.com/en-us/agent-framework/overview/
- https://learn.microsoft.com/en-us/agent-framework/workflows/overview
- https://learn.microsoft.com/en-us/agent-framework/agents/tools/overview
- https://learn.microsoft.com/en-us/agent-framework/integrations/overview
- https://github.com/microsoft/agent-framework

### CrewAI

- https://docs.crewai.com/en/introduction
- https://docs.crewai.com/en/concepts/crews
- https://docs.crewai.com/en/concepts/flows
- https://docs.crewai.com/en/concepts/memory
- https://docs.crewai.com/en/mcp/overview
- https://docs.crewai.com/en/learn/human-in-the-loop
- https://docs.crewai.com/en/observability/overview
- https://github.com/crewAIInc/crewAI

### Vercel AI SDK

- https://ai-sdk.dev/docs/introduction
- https://ai-sdk.dev/docs/agents/overview
- https://ai-sdk.dev/docs/ai-sdk-core/provider-management
- https://ai-sdk.dev/docs/ai-sdk-core/tools-and-tool-calling
- https://ai-sdk.dev/docs/ai-sdk-core/mcp-tools
- https://ai-sdk.dev/docs/agents/memory
- https://ai-sdk.dev/docs/ai-sdk-core/telemetry
- https://ai-sdk.dev/docs/ai-sdk-core/testing
- https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol
- https://github.com/vercel/ai

### Mastra

- https://mastra.ai/docs/agents/overview
- https://mastra.ai/docs/workflows/overview
- https://mastra.ai/docs/mcp/overview
- https://mastra.ai/docs/memory/overview
- https://mastra.ai/docs/observability/overview
- https://mastra.ai/docs/evals/overview
- https://mastra.ai/docs/deployment/overview
- https://github.com/mastra-ai/mastra
