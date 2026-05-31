# Contributing

Thanks for your interest in Grateful Agents SDK. The project is GPLv3 because
we don't want agent runtimes to become a proprietary moat — contributions are
welcome under the same license.

## Before you open a PR

1. **Open an issue first** for non-trivial changes so we can agree on scope.
2. Make sure your change is in scope. The SDK is intentionally focused on:
   - A provider-neutral agent loop in Go.
   - Tool, guardrail, sandbox, MCP, sub-agent, handoff, and tracing primitives.
   - Security-by-default behavior (see [docs/security.md](docs/security.md)).
3. Security-sensitive changes (sandbox, guardrails, MCP, web/SSRF, secrets,
   tool policy) require a regression test that fails before your fix and
   passes after.

## Development

```bash
# Requires Go 1.26.2+
go version

# Run tests
GRATEFUL_LIVE_TESTS=skip go test ./...

# Full verification (tests + vet + govulncheck)
make verify
```

## Style

- Standard `gofmt` / `go vet` — no extra linters required.
- Keep public API in `pkg/agentsdk` minimal. Internals belong in `internal/`.
- Prefer Go stdlib over third-party deps. New transitive deps should be
  justified in the PR description.
- Comments only where they clarify non-obvious behavior.

## Tests

- Unit tests next to the code (`*_test.go`).
- Integration tests in `test/integration/`.
- Worked examples that double as tests live in `examples/features/`.
- Security regression corpora live in `eval/audit-fixtures/`. New
  jailbreak / SSRF / secret patterns are very welcome there.

## Commit messages

- One logical change per commit.
- Imperative mood subject line, ≤72 chars.
- Reference issues / advisories in the body when relevant.

## Reporting security issues

Do **not** file a public issue. See [SECURITY.md](SECURITY.md).

## License

By contributing you agree your contribution will be licensed under
GPL-3.0-only, the same license as the project.
