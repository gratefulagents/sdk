// Package modelactivity carries an optional per-call model stream activity
// callback through context. Transports that consume provider streams internally
// use it to report progress even when individual stream events are not exposed
// to the agent runner.
package modelactivity

import "context"

type sinkKey struct{}

// Sink is called whenever a provider response stream makes progress.
// Implementations must be safe for concurrent use and must return quickly.
type Sink func()

// WithSink returns a context that reports model stream activity to sink.
func WithSink(ctx context.Context, sink Sink) context.Context {
	if sink == nil {
		return ctx
	}
	return context.WithValue(ctx, sinkKey{}, sink)
}

// Notify reports model stream activity to the sink installed on ctx, if any.
func Notify(ctx context.Context) {
	if ctx == nil {
		return
	}
	if sink, ok := ctx.Value(sinkKey{}).(Sink); ok && sink != nil {
		sink()
	}
}
