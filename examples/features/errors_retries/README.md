# Errors And Retries

This example shows model retry advice, typed errors, run error handlers, and `RunConfig.RetryPolicy`.

Run it:

```sh
go test ./examples/features/errors_retries
```

No credentials required. This suite intentionally uses a scripted fake
model rather than a live provider — the live API cannot deterministically
inject the transient/retryable failures these tests verify. See the
top-level README [Running tests](../../../README.md#running-tests) section
for the full env table.

How to use this feature:

- Return `ModelRetryAdvice{ShouldRetry: true}` from your model when a provider error should be retried.
- Configure `RunConfig.MaxTurns` with enough turns for retry attempts.
- Use `RunConfig.ErrorHandler` when the application wants to retry, continue, or abort on its own terms.
- Set `RunConfig.RetryPolicy` to apply SDK-level retry behavior when the provider does not return retry advice.
- Use `errors.As` with SDK typed errors such as `AgentError`, `MaxTurnsExceeded`, `ToolTimeoutError`, and guardrail tripwire errors.
- Use `agentsdk.DefaultRetryPolicy()` and `DelayForAttempt` when you need the same backoff logic outside the runner.

Runnable source: [errors_retries_test.go](errors_retries_test.go).
