# Coding-agent tool surface

The canonical runtime builder exposes the SDK's built-in coding tools through `runtime.Config.Features.Tools`. When `Config.Features` is non-nil, selection is strict: every family is disabled unless its field is true. A nil `Features` preserves the legacy core surface, but does not implicitly enable Browser, Vision, Terminal, Think, or GitHub mutation tools.

## Complete, pageable search

`glob` and `grep` preserve their text-oriented compatibility mode while making incomplete output explicit. Text results include completion metadata whenever a limit is reached. Set `output_format: "json"` for a stable envelope containing `matches`, `truncated`, and `next_cursor`; pass that cursor back with the same query to retrieve the next deterministic page. A cursor is rejected if its query options do not match.

Both tools accept multiple `include` and `exclude` globs and explicit `respect_gitignore` behavior. Built-in directory skipping is controlled separately. `grep` additionally supports `before_context`, `after_context`, and `mode: "matches" | "files" | "count"`. Ordering is deterministic while the workspace is unchanged. Limits apply to returned records and bounded output, including long lines and context.

Compatibility: callers that omit `output_format` continue to receive text. They should no longer parse a limited response as complete without checking the appended truncation metadata. New integrations should use JSON and cursors.

## Patch and path lifecycle tools

Write-capable permission modes register three structured mutation tools:

- `ApplyPatch` accepts `patch` (a bounded unified Git-style diff) and optional `dry_run`. It supports file creation, modification, deletion, rename metadata, and executable-bit changes. The complete patch, all paths, source contents, modes, and hunks are validated before mutation. Unsupported directives (including copy and binary patches) fail closed. Results are JSON with `dry_run`, per-file `operations`, and a bounded human-readable `diff`.
- `Move` atomically moves or renames a regular file or directory to a non-existing workspace path. It uses no-clobber, descriptor-relative operations and rejects symlinks and hard-linked files.
- `Delete` removes a regular file or an empty directory. Non-empty trees must be deleted explicitly from the leaves upward; recursive deletion is intentionally unavailable because it cannot provide the same race, confinement, and audit guarantees.

All affected paths are resolved before an `ApplyPatch` mutation begins. Existing files must be regular, singly linked, bounded UTF-8 files. Dry-run performs the same complete validation but does not mutate disk. Writes use same-directory staging and atomic replacement; multi-file failures use a mutation journal and best-effort rollback without overwriting paths changed concurrently. Secure lifecycle operations fail closed on platforms where descriptor-relative confinement is unavailable.

The tools are unavailable in ordinary read-only mode unless a host explicitly allowlists their exact names through `AllowedMutatingTools`.

## Generic read-only LSP

`LSP` is a generic stdio JSON-RPC language-server client rather than a `gopls` CLI wrapper. `lsp.Config` accepts a trusted server ID, command, arguments, environment, language ID, and file selectors plus startup/request/message/output bounds. `AdditionalServers` configures polyglot routing in one tool; a host-owned `ServerDiscoverer` can return trusted candidates dynamically. Selection by language and path must resolve to exactly one server.

Supported read-only operations are:

- definitions (`definition` and legacy `goToDefinition`)
- references (`references` and legacy `findReferences`)
- hover
- document and workspace symbols
- implementations and type definitions
- pull diagnostics and bounded published diagnostics

Requests and structured results use one-based lines and one-based UTF-16 code-unit character positions, making returned positions reusable as inputs. Locations and diagnostics are filtered to the configured workspace. Document reads use the hardened no-follow/single-link boundary, and changed open documents are synchronized with `didChange`.

Language servers run through the configured SDK subprocess executor in read-only mode. Registry/runtime construction always injects that sandbox and owns the manager as a bundle closer. Standalone use fails closed without an executor unless the caller explicitly sets `AllowUnsafeUnconfined` for a trusted process. Sessions have bounded startup/request timeouts, protocol messages, stderr, output, and message queues; close terminates and reaps the process tree.

Server requests that could mutate state, including `workspace/applyEdit`, are explicitly rejected. Rename, formatting, code actions, and workspace-edit application are not exposed.

A zero-command configuration retains the Go migration path by selecting `gopls`, `go`, and `*.go`; legacy operation aliases remain accepted. Output is now structured JSON rather than raw `gopls` CLI text.

Example polyglot configuration:

```go
cfg.LSPConfig = lsp.Config{
    AdditionalServers: []lsp.Config{
        {ID: "typescript", Command: "typescript-language-server", Args: []string{"--stdio"}, LanguageID: "typescript", FilePatterns: []string{"*.ts", "*.tsx"}},
        {ID: "python", Command: "pyright-langserver", Args: []string{"--stdio"}, LanguageID: "python", FilePatterns: []string{"*.py"}},
    },
}
```

The SDK configures and confines servers; it does not bundle language-server binaries.

## Runtime-built-in families

| Registry family | Strict feature | Prerequisites |
| --- | --- | --- |
| Workspace search (`list_files`, `read_file`, `glob`, `grep`) | `ListFiles`, `ReadFile`, `Glob`, `Grep` | None |
| Workspace mutation (`Write`, `Edit`, `ApplyPatch`, `Move`, `Delete`) | matching fields | Write permission mode |
| LSP | `LSP` | `LSPConfig`; zero command uses the Go compatibility default |
| Bash | `Bash` | Permission mode selects the Bash variant |
| Web fetch | `WebFetch` | Network policy applies |
| Async shell | `AsyncShell` | Write permission mode |
| Signals | individual `Signals` fields | None |
| Browser | `Browser` | `AllowPrivateNetworkURLs` must be true because Chromium cannot enforce the public-only policy across redirects/subresources |
| Vision | `Vision` | Inject an analyzer or use an eligible OpenAI provider adapter |
| Interactive terminal | `InteractiveTerminal` | Danger-full-access and enabled Git remote writes |
| Think | `Think` | None |
| Attach repository | `AttachRepository` | Write permission mode |
| GitHub pull request | `GitHubPullRequest` | Write permission mode and enabled Git remote writes |
| GitHub issue | `GitHubIssue` | Write permission mode |

Browser screenshot output is configured with `BrowserScreenshotDir`. GitHub tools accept `GitHubCommandRunner` and `GitHubArtifactSink`. Registry permission checks, PR remote-write filtering, URL policy, and all session/process closers remain active through builder wiring. Strict name filtering is deterministic and never enables an unselected sibling family.

`RegistryCapabilities` is the canonical classification manifest. A static parity test checks every capability-producing `RegistryOption` against that manifest, and runtime tests require every runtime-built-in manifest family to have feature wiring. Adding a registry family without a runtime-built-in, host-only, or configuration-only classification fails tests.

## Intentionally host-only families

`Memory` is intentionally host-only. It requires a caller-owned durable `memory.Store` plus host-specific namespace, run, and repository identity; the generic runtime cannot safely invent these values. Hosts can use the supported `ExtraTools` extension path:

```go
memoryTool := memory.New(store, namespace, sourceRun, repoURL)

bundle, err := runtime.BuildToolBundle(ctx, runtime.Config{
    WorkDir: "/workspace/repo",
    Features: &runtime.Features{
        Tools: runtime.ToolFeatures{ExtraTools: true},
    },
    ExtraTools: []agentsdk.Tool{memoryTool},
})
```

The same path supports host-specific tools and adapters that are not SDK registry capabilities.
