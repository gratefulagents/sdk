# Durable runs and recovery

The SDK can optionally checkpoint an exact run at model, tool, approval,
handoff, child, pause, and completion boundaries. Existing `Runner.Run` callers
need no store and retain the original in-process behavior.

## Contract

`agentsdk.DurableRunConfig` emits versioned, function-free JSON checkpoints.
Checkpoint persistence is **fail closed**: a model or tool is not dispatched if
its prepared checkpoint cannot be stored, and execution does not cross a
completed boundary until that checkpoint is durable. Stable run, attempt, step,
tool-call, approval, effect, and child-run IDs are available in
`agentsdk/durable`.

The `durable.RunStore` contract provides immutable ordered events, compact
snapshots, compare-and-swap revisions, and fenced expiring leases. Reference
implementations are provided for private filesystem storage and PostgreSQL.
`agentsdk.OpenStoredRun` connects any `RunStore` to `Runner`:

```go
store, err := durable.NewFilesystemStore("/var/lib/myapp/runs")
if err != nil { /* handle */ }
run, err := agentsdk.OpenStoredRun(ctx, store, agentsdk.StoredRunOptions{
    TenantID: durable.TenantID("tenant_acme"),
    Owner: "worker-7",
})
if err != nil { /* handle */ }
defer run.Close(ctx)

cfg := agentsdk.RunConfig{Durable: run.RunConfig()}
result, err := runner.Run(ctx, agent, input, cfg)
```

After a restart, open the same tenant/run ID. `RunConfig()` supplies the latest
continuation and a new attempt ID. Cumulative budget values are carried forward
rather than reset with the process. Two workers cannot acquire the same
unexpired run lease, and every checkpoint update also uses snapshot CAS.

## External effects

External effects use a persisted protocol:

1. `prepared` — intent and stable idempotency key are durable;
2. `dispatched` — the request may have reached its destination;
3. `succeeded` or `failed` — a definite outcome is durable;
4. `outcome_unknown` — dispatch occurred but its outcome cannot be proved.

Classify each effect as `idempotent`, `deduplicated`, or `non_replayable`.
Propagate `Effect.IdempotencyKey` to destinations that honor it. During a durable `Runner` tool call, the same stable value is available through `agentsdk.DurableIdempotencyKeyFromContext(ctx)`. An idempotent
or destination-deduplicated effect can be retried with that same key. A
non-replayable effect in `outcome_unknown` always returns
`operator_resolution` from `durable.RecoverEffect`; the SDK never retries it
automatically. Checkpoints containing unresolved prepared model/tool or approval
work similarly require reconciliation before `Runner` resumes.

This is not an exactly-once claim. Exactly-once behavior is only possible when
the destination honors the stable idempotency key or participates in the same
transaction as the run store.

## Child runs and cancellation

Snapshots contain first-class child run records and durable cancellation
requests. Scheduler tasks that were active when a process stopped restore as
`reconciling`, not generic failures. A durable child worker may finish them, or
an operator can record a terminal decision with
`SubAgentRegistry.ReconcileRestoredTask`. No unknown child outcome is silently
replayed.

## Security and lifecycle

Every key includes a tenant ID. Stores reject unsafe filesystem IDs and scope
all SQL operations by tenant and run. Payloads carry `DataClassification`.
Configure a `Redactor` to transform fields before persistence and an `Encryptor`
to protect complete records with caller-managed keys. Filesystem records and
directories use private permissions and atomic replacement. The filesystem
store uses cross-process advisory locks on supported Unix platforms; use the
PostgreSQL store when cross-process fencing is required on platforms without
`flock`.

`DeleteRun`, `DeleteTenant`, and `ApplyRetention` provide explicit deletion and
retention. Applications remain responsible for backup deletion, key rotation,
and any regulatory policy outside the reference store.

## Schema evolution

Current durable documents use schema v2. `durable.DecodeDocument` performs
explicit migrations, including the shipped v1-to-v2 migration. Unknown future
versions fail closed rather than being interpreted as current state.
