// Package modelactivity carries an optional per-call model stream activity
// callback through context. Transports that consume provider streams internally
// use it to report progress even when individual stream events are not exposed
// to the agent runner.
package modelactivity

import (
	"context"
	"io"
	"net/http"
)

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

// WrapTransport reports activity whenever bytes arrive from a provider HTTP
// response. Observing the body at this boundary includes SSE events that a
// provider SDK may intentionally discard, such as heartbeat pings.
func WrapTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return activityRoundTripper{base: base}
}

type activityRoundTripper struct {
	base http.RoundTripper
}

func (t activityRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &activityReadCloser{ReadCloser: resp.Body, ctx: req.Context()}
	return resp, nil
}

type activityReadCloser struct {
	io.ReadCloser
	ctx context.Context
}

func (r *activityReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		Notify(r.ctx)
	}
	return n, err
}
