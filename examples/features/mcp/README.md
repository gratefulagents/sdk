# MCP

The SDK supports local stdio MCP servers, an experimental read-only remote
client, and policy-gated Streamable HTTP server mode.

## Local stdio

This example shows how an SDK client exposes MCP descriptors as `agentsdk.Tool`
values with `pkg/agentsdk/mcp.BuildTools`:

```sh
go test ./examples/features/mcp
```

An omitted `type` remains compatible with `stdio`.

## Experimental remote client

Remote configuration uses `type: "streamable-http"` and `url`. Repository
configuration never grants network or tool access by itself. A trusted host
must opt in to each server and classify exact raw tool names as read-only:

```go
manager, err := mcp.NewManager(ctx, workDir,
    mcp.WithRemoteServers("catalog"),
    mcp.WithRemoteReadOnlyTools("catalog", "search", "get_item"),
    mcp.WithRemoteTenant(tenantID),
    mcp.WithRemoteHeaderProvider(headers),
)
```

Remote tools are exposed only when all three controls agree: the host
read-only allowlist, `.mcp.json`'s `trustReadOnlyHint`, and the server's MCP
`readOnlyHint`. Mutating remote tools are not registered. Private destinations
and plain HTTP are rejected unless the trusted host explicitly names an
intranet/test server with `WithPrivateNetworkRemoteServers`.

`WithRemoteOAuth(serverName, ...)` binds distinct audience/scope policy to each
server and validates claims and expiry on every request. `WithRemoteRootCAs`
adds explicit trust roots without disabling TLS verification. Credentials are
obtained per request and are never stored in `.mcp.json`.
`WithRemoteAuditHook` records credential-free tenant, server, operation, and
outcome provenance and fails closed when the sink is unavailable
before execution.

Remote `tools/call` requests are never replayed. If a response may have been
lost after execution, the manager returns an error matching
`mcp.ErrOutcomeUnknown`; reconcile it rather than retrying blindly.

Legacy SSE is available only through the explicit `type: "sse"` compatibility
mode. Streamable HTTP is preferred.

## Server mode

`mcp.NewServerMode` exposes an explicit list of SDK tools through a Streamable
HTTP handler. A tenant resolver and `ServerToolPolicy` are mandatory. Server
mode never calls `Tool.Execute` directly: the host policy executor must apply
the same access checks, approvals, guardrails, tracing, quotas, and audit hooks
as native SDK execution. Each policy request carries a SHA-256 digest binding
approval and audit records to the immutable tenant, tool name, and arguments.
`WithServerResources` and `WithServerPrompts` add explicitly selected protocol
surfaces whose callbacks also receive the resolved tenant. Sessions are bound to
their authenticated tenant, idle-expire, and have global and per-tenant caps;
call `mode.Close()` during shutdown.

Serve `ServerMode.Handler()` behind authenticated, verified TLS. Agents adapted
as SDK tools can use the same policy-gated path.
