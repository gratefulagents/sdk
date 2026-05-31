# Memory Example

Demonstrates the public `pkg/agentsdk/memory` primitives: `InMemoryStore`,
tag/namespace filtering, a custom `Store` implementation, and the deterministic
`NoopEmbedder`.

`InMemoryStore` is appropriate for tests and simple host integrations. For
durable semantic search, plug in your own `Store` implementation backed by a
vector database; the `Store` interface (`Store`, `Search`, `List`, `Delete`) is
the only contract the SDK depends on.

For a concrete durable-store sketch, see
[postgres_pgvector.md](postgres_pgvector.md). It includes a pgvector schema,
a `PostgresStore` implementation, and examples for registering it through both
`tools.WithMemoryStore` and `runtime.Config.ExtraTools`.

Run:

```sh
go test ./examples/features/memory
```
