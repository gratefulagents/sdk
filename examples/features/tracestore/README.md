# Trace Store Example

Demonstrates the public `pkg/agentsdk/tracestore.FilesystemTraceStore`: per-run
directories, NDJSON trace appenders, score files, and run listing/filtering.

`FilesystemTraceStore` is the on-disk persistence target the SDK uses to make
runs auditable and replayable; it pairs with the `tracing.Processor` and
`EventStream` plumbing exercised in the `observability` example.

Run:

```sh
go test ./examples/features/tracestore
```
