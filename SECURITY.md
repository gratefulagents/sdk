# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report privately via GitHub's **[private vulnerability reporting](https://github.com/gratefulagents/sdk/security/advisories/new)**.

Include:

- A description of the issue and its impact.
- Steps to reproduce (a minimal Go reproducer is ideal).
- Affected versions / commit SHA.
- Any suggested mitigation.

We aim to acknowledge reports within **3 business days** and to ship a fix or
mitigation in a timely manner depending on severity.

## Scope

In scope:

- The Go SDK in `pkg/agentsdk/...` and `internal/agent/...`.
- The `cmd/grateful-agent-run` headless runner.
- Tool implementations in `pkg/agentsdk/tools/...`.
- The bubblewrap subprocess sandbox in `pkg/agentsdk/sandbox`.
- MCP integration in `pkg/agentsdk/mcp`.

Out of scope (see [docs/security.md](docs/security.md) "Non-goals"):

- Compromised host kernel or OS.
- Malicious model providers.
- Side-channel attacks on the host.
- Supply-chain attacks against Go module dependencies (please report these
  upstream).
- The `LocalExecutor` running untrusted commands without the subprocess
  sandbox — that is documented as an opt-in unsafe mode.

## Threat model & defenses

See **[docs/security.md](docs/security.md)** for the full threat model,
defense-in-depth controls (sandbox, SSRF blocks, secret detection,
prompt-injection tagging, fail-closed tool access, MCP descriptor handling,
bounded reads, retry caps, trace permissions), and known non-goals.

## Public disclosure

Once a fix is merged and a release is tagged, we will:

1. Publish a GitHub Security Advisory (with CVE if appropriate).
2. Credit the reporter (unless they prefer anonymity).
3. Note the fix in release notes.
