// Package modeldelta carries an optional per-call sink for streamed model
// reasoning text through context. Transports that already consume streaming
// HTTP responses inside blocking calls (GetResponse) can surface reasoning
// deltas live to the host without changing their public signatures.
//
// It is a leaf package so that internal/agent (which installs sinks) and the
// provider transports internal/anthropic and internal/openai (which invoke
// them) can all import it without cycles.
package modeldelta

import "context"

// ReasoningSink receives incremental reasoning text chunks in stream order.
// Implementations must be cheap and non-blocking: they are called inline from
// the goroutine that consumes the model's HTTP stream. Chunks may be re-sent
// from the start if the transport retries a failed request mid-stream;
// consumers reconcile via the final complete reasoning text.
type ReasoningSink func(text string)

type ctxKey struct{}

// WithReasoningSink returns a context that delivers streamed reasoning text
// chunks to sink during model calls made with this context.
func WithReasoningSink(ctx context.Context, sink ReasoningSink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, ctxKey{}, sink)
}

// ReasoningSinkFromContext extracts the reasoning sink installed by
// WithReasoningSink, or nil when the call has no live reasoning consumer.
func ReasoningSinkFromContext(ctx context.Context) ReasoningSink {
	if ctx == nil {
		return nil
	}
	sink, _ := ctx.Value(ctxKey{}).(ReasoningSink)
	return sink
}
