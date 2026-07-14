package modelactivity

import (
	"context"
	"io"
	"net/http"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type chunkReadCloser struct {
	chunks [][]byte
}

func (r *chunkReadCloser) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks = r.chunks[1:]
	return n, nil
}

func (*chunkReadCloser) Close() error { return nil }

func TestWrapTransportReportsEveryResponseBodyRead(t *testing.T) {
	activityCount := 0
	ctx := WithSink(context.Background(), func() { activityCount++ })
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.test/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &chunkReadCloser{chunks: [][]byte{
		[]byte("event: message_start\ndata: {}\n\n"),
		[]byte("event: ping\ndata: {}\n\n"),
		[]byte("event: input_json_delta\ndata: {}\n\n"),
	}}
	transport := WrapTransport(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body}, nil
	}))

	resp, err := transport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatal(err)
	}
	if activityCount != 3 {
		t.Fatalf("activity count = %d, want one notification for each of 3 provider chunks", activityCount)
	}
}
