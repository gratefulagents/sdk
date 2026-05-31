# Security

This document describes the security posture of the gratefulagents SDK: which
attacker classes the runtime defends against, the concrete mitigations
shipped, and (importantly) what the SDK explicitly does **not** defend
against. Security regression corpora live under
[`eval/audit-fixtures/`](../eval/audit-fixtures/).

> Threat model in one line: *the model and its tool outputs are
> semi-trusted; the host process running the SDK is trusted.* Most defenses
> aim to keep a misbehaving or compromised model from escalating its blast
> radius beyond what the host policy already allows.

## Defenses shipped in the SDK

### Trace store on disk

Run traces (LLM requests, tool I/O, scores) can carry secrets — prompts,
tool outputs, agent reasoning. The filesystem trace store now creates run
directories with mode `0o700` and individual files with mode `0o600`, so
non-owner local users cannot read them. See
`pkg/agentsdk/tracestore/trace_store.go` and the `TestFilesystemTraceStorePermissions`
regression test.

### MCP transport hardening

`.mcp.json` configures Model Context Protocol servers (subprocess plus
environment). Two abuse vectors are addressed:

- **Credential leakage into MCP children.** The inherited environment is
  filtered against a credential denylist (AWS, GitHub, OpenAI, Anthropic,
  Slack, GCP, generic `*_TOKEN`/`*_SECRET`/`*_PASSWORD` patterns). Hosts that
  legitimately need to forward a credential opt in by listing it in
  `MCPServer.AllowEnv`. See `pkg/agentsdk/mcp/`.
- **Configuration tampering.** The `.mcp.json` path is pinned at chatloop
  start and reload semantics are documented; the server set cannot mutate
  the loaded configuration mid-run.

### Tool output as untrusted input

Tool results are spliced back into the next turn's model input, so prompt
injection in a fetched webpage / file becomes a model-level vulnerability.
By default the runner wraps tool output in
`BEGIN UNTRUSTED TOOL OUTPUT … END UNTRUSTED TOOL OUTPUT` markers and
instructs the model to treat that span as data, not instructions. Hosts can
disable this for a specific surface via `RunConfig.UntrustedToolOutputs =
ptr(false)`, but the default is **on**.

### Access configuration fails closed

Tool access has two enforcement tiers: `full` and `read-only`. Empty access
keeps the historical default (`full`), but any unknown non-empty value is
normalized to `read-only` inside the runner. The headless CLI rejects unknown
`--tool-access` values outright. A typo such as `fulll` therefore cannot
silently expose write tools.

### Sub-agent access clamping

A sub-agent task spawned by a read-only parent cannot escalate its
`ToolAccessLevel` to `full` — the runner clamps the child's access to the
parent's level and emits a warning. This prevents "spawn a child with full
access" from being a free privilege-escalation primitive for any tool
exposed to read-only contexts.

Read-only filtering for `agent_*` tools is type-based. A real `Agent.AsTool`
sub-agent may be treated as control flow, but an ordinary custom tool with a
misleading `agent_` name is still filtered and approval-gated like any other
mutating tool.

### URL fetching: SSRF, IPv6, decompression bombs

`pkg/agentsdk/tools/web/url_security.go` validates every dial target — both
on the initial fetch and on every redirect — against:

- IPv4 private/loopback/link-local/CGNAT/benchmark blocks.
- IPv6 metadata-relevant ranges: unique local (`fc00::/7`, including
  `fd00:ec2::254`), link-local, multicast, NAT64 (`64:ff9b::/96` and
  RFC 8215 local-use `64:ff9b:1::/48`, which can wrap private IPv4),
  documentation, and the discard prefix.
- Hostname blocklist (`localhost`, `*.localhost`, `*.local`,
  `metadata.google.internal`).

Safe HTTP clients also disable transport-level compression
(`Transport.DisableCompression = true`) so that any caller-imposed
`io.LimitReader` cap applies to wire bytes — gzip/deflate/br decompression
bombs cannot inflate past the cap.

### MCP descriptors as untrusted text

MCP server-supplied titles and descriptions are not trusted operator or system
text. Human-display strings are flattened, control-character-free,
length-bounded, and provenance-tagged. Model-facing dynamic tool descriptions
are also flattened and bounded, and explicitly frame the server-supplied
description as untrusted descriptive data rather than instructions.

### Sandbox executor: fail-closed read-only

When a tool is invoked under `ToolAccessLevelReadOnly`, the executor selects
a read-only-capable sandbox if one is available, but if no enforcing backend
is available it now **fails closed** rather than silently downgrading to a
permissive backend. The `LocalExecutor` is intentionally permissive and is
not selected automatically in this mode.

### Subprocess lifecycle

Long-running tools (shell, sandboxed exec, MCP children) run in a dedicated
process group on Unix. On timeout or context cancel, the runtime sends
`SIGTERM` to the entire group, waits a short grace period, then escalates
to `SIGKILL` — preventing detached children from outliving the agent run.
On Windows the equivalent `Kill()` cleanup is used.

### Output cap without kill-on-exceed

Tool execution streams stdout/stderr through capped writers. The Bash tool
uses a 100 KiB cap; direct `sandbox.Executor.Run` calls use a 1 MiB cap for
callers such as Browser and LSP. When the byte budget is exhausted, the
wrapper keeps the beginning and latest tail of the stream and discards the
middle while the process continues. Timeouts still terminate the process group,
so verbose legitimate commands can finish without allowing unbounded output to
exhaust agent memory.

### Bounded local reads

Read-oriented tools cap local file/image reads before buffering content in
memory. `read_file` returns at most 100 KiB, image loading rejects files larger
than 20 MiB before reading them, and exact file editing rejects files larger
than 5 MiB. These are availability controls, not confidentiality controls.

### Provider retry caps

Provider `Retry-After` values and runner retry delays are capped at 5 minutes.
A malicious or broken provider response cannot stall a run for hours by sending
an extreme retry value.

### Browser subprocess isolation

The built-in Browser tool launches Chromium through a sandbox executor by
default, including screenshot mode. On systems without an enforcing sandbox
(for example macOS without a host-supplied executor), browser actions fail
closed instead of running an unsandboxed browser against model-supplied URLs.

### Private local artifacts

Trace artifacts, CLI event logs, and MCP binary blobs can contain prompts,
tool outputs, screenshots, or resource bytes. The SDK writes trace
directories and MCP blob directories with `0o700`, and writes trace files,
event logs, screenshots, and MCP blobs with `0o600`.

### Secret detection

Both input and output guardrails run a curated set of high-confidence
credential signatures against tool I/O (see
`pkg/agentsdk/guardrails/builtin.go`). Coverage:

- AWS access keys including STS temporary credentials (`AKIA…`, `ASIA…`).
- GitHub token variants (`ghp_`, `ghs_`, `ghu_`, `ghr_`, `gho_`).
- OpenAI project keys (`sk-proj-…`).
- Anthropic API keys (`sk-ant-…`, including the `api03` version segment).
- Slack tokens (`xoxb`/`xoxp`/`xoxa`/`xoxr`/`xoxs`).
- npm automation tokens.
- JSON Web Tokens (three base64url segments, anchored on `eyJ…`).
- PEM-encoded private keys, including OPENSSH and PKCS#8.
- HTTP `Authorization: Bearer …` headers and standalone `Bearer <token>`.
- GCP service-account JSON markers.
- Generic `api_key`/`secret_key`/`password` literal assignments in source.

Patterns are regex-only and RE2-compatible (no backtracking).

### Shell denylist tokenizer

Destructive command detection in
`pkg/agentsdk/tools/shell/classifier.go` and the input guardrail share a
tokenizer that strips shell quoting/escaping before matching. It defeats
common evasions:

- Backslash escapes such as `\rm -rf /`.
- IFS expansion (`${IFS}`) and other parameter expansions used as separators.
- ANSI-C quoting (`$'\x2d'`, `$'\x72\x6d'`, etc.).
- Command substitution with `$()` and backticks.
- Wrapper invocations such as `bash -c …`, `sh -c …`, `env – sh …`.

The fixture `eval/audit-fixtures/cmd_obfuscation.txt` exercises these and
related variants.

## Non-goals — what this SDK does **not** defend against

The SDK is a *runtime library*, not a hypervisor or a sandbox in its own
right. The following are explicitly out of scope:

- **A compromised host kernel or operating system.** Process-group kills,
  `O_NOFOLLOW`, file-mode bits, and similar primitives all rely on the
  kernel behaving correctly.
- **A malicious model provider.** If the LLM provider returns crafted
  responses *and* exfiltrates the agent's prompt/tool-output stream, no
  in-process check can prevent that. Use a provider you trust (or a local
  model).
- **Side-channel attacks.** Timing, cache, and resource-exhaustion side
  channels against co-tenant workloads are not addressed.
- **Supply-chain attacks on Go module dependencies.** Versions are pinned in
  `go.mod`/`go.sum`, but a compromised upstream module would not be detected by
  the SDK itself; verify with module checksums and your own vetting.
- **The `LocalExecutor` running untrusted code.** `LocalExecutor` is
  intentionally permissive: it executes commands with the host's full
  privileges. It exists for development and trusted-workspace use cases.
  For untrusted code (especially anything driven directly by the model)
  use the bubblewrap-based subprocess sandbox in
  `pkg/agentsdk/sandbox`, or run the entire agent in an external sandbox
  (container, VM).
- **Denial of service via excessive but non-output resource use.** CPU and
  RSS caps on tool subprocesses are the host's responsibility (cgroups,
  ulimit, etc.). The SDK caps output bytes and wall-clock, not CPU time.
- **Network egress filtering beyond SSRF.** SSRF blocks aim at private/
  metadata destinations. General DLP — preventing the model from sending
  prompt content to attacker-controlled public hosts — is a host-policy
  concern, e.g. an outbound HTTP proxy.
- **Secret storage in ordinary workspace files.** File-writing tools create
  normal project files using ordinary workspace permissions. Do not treat
  model-written source files, notes, generated artifacts, or installed skill
  assets as a secure secret store.
- **Filesystem confidentiality across users on the same host.** Sensitive
  SDK-created artifacts use owner-only file modes, which protects against
  other unprivileged users on the same machine. Root, the artifact owner,
  and any process sharing the agent's UID are still trusted.

## Reporting a vulnerability

Please open a private security advisory on the GitHub repository rather
than a public issue. Include a reproduction (a minimal `agentsdk` test or
fixture under `eval/audit-fixtures/` is ideal) and the expected vs. actual
behavior.
