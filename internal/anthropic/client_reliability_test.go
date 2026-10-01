package anthropic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/gratefulagents/sdk/internal/modeldelta"
)

const reliabilityStartSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"
const reliabilityStopSSE = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
const reliabilityCompleteSSE = reliabilityStartSSE + reliabilityStopSSE

func reliabilityErrorSSE(kind string) string {
	return fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":%q,\"message\":\"test failure\"}}\n\n", kind)
}

type reliabilityTransport func(*http.Request) (*http.Response, error)

func (f reliabilityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reliabilityClient(wire reliabilityTransport, opts ...Option) *Client {
	c := NewClient("test-key", opts...)
	c.sdk = sdk.NewClient(append(c.sdk.Options, option.WithHTTPClient(&http.Client{Transport: wire}))...)
	return c
}

func reliabilityResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

func reliabilityRequest() CreateMessageRequest {
	return CreateMessageRequest{Model: "claude-sonnet-4-5", MaxTokens: 8, Messages: []Message{{Role: RoleUser, Content: []ContentBlock{NewTextBlock("hello")}}}}
}

func TestReliabilityAuthIgnoresEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "environment-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "environment-token")
	for _, tc := range []struct {
		name, key, auth string
		opts            []Option
	}{
		{name: "api key", key: "test-key"},
		{name: "oauth", auth: "Bearer oauth", opts: []Option{WithOAuthToken("oauth")}},
		{name: "source", auth: "Bearer source", opts: []Option{WithOAuthTokenSource(&staticTokenSource{token: "source"})}},
		{name: "empty source", opts: []Option{WithOAuthTokenSource(&staticTokenSource{})}},
		{name: "source fallback", auth: "Bearer fallback", opts: []Option{WithOAuthToken("fallback"), WithOAuthTokenSource(&staticTokenSource{})}},
		{name: "gateway", auth: "Bearer gateway", opts: []Option{WithBearerToken("gateway")}},
		{name: "gateway headers", auth: "Bearer header", opts: []Option{WithBearerToken("gateway"), WithRequestHeaderProvider(func(context.Context) (map[string]string, error) {
			return map[string]string{"Authorization": "Bearer header"}, nil
		})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
				if key, auth := r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"); key != tc.key || auth != tc.auth {
					t.Errorf("auth headers = (%q, %q), want (%q, %q)", key, auth, tc.key, tc.auth)
				}
				return reliabilityResponse(r, 200, reliabilityCompleteSSE), nil
			}, tc.opts...)
			if _, err := c.CreateMessage(context.Background(), reliabilityRequest()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReliabilityIncompleteStreams(t *testing.T) {
	for _, body := range []string{"", reliabilityStartSSE} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/empty=%v", streaming, body == ""), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := 0
					c := reliabilityClient(func(r *http.Request) (*http.Response, error) { calls++; return reliabilityResponse(r, 200, body), nil })
					var err error
					if streaming {
						r, openErr := c.CreateMessageStream(context.Background(), reliabilityRequest())
						if openErr != nil {
							t.Fatal(openErr)
						}
						_, err = r.CollectResponse()
						if _, again := r.Next(); !errors.Is(again, io.ErrUnexpectedEOF) {
							t.Fatalf("terminal error = %v", again)
						}
					} else {
						_, err = c.CreateMessage(context.Background(), reliabilityRequest())
					}
					if !errors.Is(err, io.ErrUnexpectedEOF) {
						t.Fatalf("error = %v", err)
					}
					wantCalls := maxRetries + 1
					if streaming {
						wantCalls = 1
					}
					if calls != wantCalls || len(c.sem) != 0 {
						t.Fatalf("calls=%d permits=%d", calls, len(c.sem))
					}
				})
			})
		}
	}
	a := NewStreamAssembler()
	if !errors.Is(a.Err(), io.ErrUnexpectedEOF) {
		t.Fatal("empty assembler succeeded")
	}
	a.Add(StreamEvent{Type: EventMessageStop})
	if err := a.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestReliabilitySSEErrorNormalization(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		status int
		retry  bool
	}{
		{"overloaded_error", 529, true}, {"rate_limit_error", 429, true}, {"api_error", 500, true}, {"unknown_error", 200, false},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", tc.kind, streaming), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					calls := 0
					c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
						calls++
						resp := reliabilityResponse(r, 200, reliabilityStartSSE+reliabilityErrorSSE(tc.kind))
						resp.Header.Set("Retry-After", "2")
						return resp, nil
					})
					var err error
					if streaming {
						r, openErr := c.CreateMessageStream(context.Background(), reliabilityRequest())
						if openErr != nil {
							t.Fatal(openErr)
						}
						_, err = r.CollectResponse()
					} else {
						_, err = c.CreateMessage(context.Background(), reliabilityRequest())
					}
					var reqErr *RequestError
					if !errors.As(err, &reqErr) || reqErr.StatusCode != tc.status || reqErr.Retryable() != tc.retry || reqErr.RetryAfterMS() != 2000 {
						t.Fatalf("error = %#v", err)
					}
					wantCalls := 1
					if !streaming && tc.retry {
						wantCalls = maxRetries + 1
					}
					if calls != wantCalls || len(c.sem) != 0 {
						t.Fatalf("calls=%d permits=%d", calls, len(c.sem))
					}
				})
			})
		}
	}
}

func TestReliabilityReasoningIsNotReplayed(t *testing.T) {
	const thinking = "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"live\"}}\n\n"
	for _, tail := range []string{"", reliabilityErrorSSE("overloaded_error")} {
		synctest.Test(t, func(t *testing.T) {
			calls, emitted := 0, 0
			c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
				calls++
				return reliabilityResponse(r, 200, reliabilityStartSSE+thinking+tail), nil
			})
			ctx := modeldelta.WithReasoningSink(context.Background(), func(string) { emitted++ })
			_, err := c.CreateMessage(ctx, reliabilityRequest())
			if err == nil || calls != 1 || emitted != 1 {
				t.Fatalf("error=%v calls=%d emitted=%d", err, calls, emitted)
			}
			if tail != "" {
				var reqErr *RequestError
				if !errors.As(err, &reqErr) || reqErr.StatusCode != 529 {
					t.Fatalf("lost typed error: %v", err)
				}
			}
		})
	}
}

func TestReliabilityRefreshHasIndependentBudget(t *testing.T) {
	for _, finalStatus := range []int{200, 401, 503} {
		t.Run(fmt.Sprint(finalStatus), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				source := &staticTokenSource{token: "old"}
				calls := 0
				c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
					calls++
					status := 503
					if calls == maxRetries+1 {
						status = 401
					}
					if calls == maxRetries+2 {
						status = finalStatus
						if r.Header.Get("Authorization") != "Bearer refreshed-token" {
							t.Error("token not refreshed")
						}
					}
					body := `{"type":"error","error":{"type":"api_error","message":"retry"}}`
					if status == 200 {
						body = reliabilityCompleteSSE
					}
					return reliabilityResponse(r, status, body), nil
				}, WithOAuthTokenSource(source))
				_, err := c.CreateMessage(context.Background(), reliabilityRequest())
				if finalStatus == 200 {
					if err != nil {
						t.Fatal(err)
					}
				} else {
					var reqErr *RequestError
					if !errors.As(err, &reqErr) || reqErr.StatusCode != finalStatus {
						t.Fatalf("error=%v", err)
					}
				}
				if calls != maxRetries+2 || source.invalidated != 1 || len(c.sem) != 0 {
					t.Fatalf("calls=%d invalidations=%d permits=%d", calls, source.invalidated, len(c.sem))
				}
			})
		})
	}
}

type reliabilityBody struct {
	io.Reader
	closes atomic.Int32
}

func (b *reliabilityBody) Close() error { b.closes.Add(1); return nil }

type reliabilityNoEOF struct{}

func (reliabilityNoEOF) Read([]byte) (int, error) { return 0, errors.New("read beyond message_stop") }

func TestReliabilityStreamPermitLifetime(t *testing.T) {
	for _, ending := range []string{"stop", "eof", "error", "close"} {
		t.Run(ending, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				body := reliabilityCompleteSSE
				if ending == "eof" {
					body = reliabilityStartSSE
				}
				if ending == "error" {
					body = reliabilityErrorSSE("overloaded_error")
				}
				tracked := &reliabilityBody{Reader: strings.NewReader(body)}
				if ending == "stop" {
					tracked.Reader = io.MultiReader(strings.NewReader(body), reliabilityNoEOF{})
				}
				var calls atomic.Int32
				c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
					n := calls.Add(1)
					resp := reliabilityResponse(r, 200, reliabilityCompleteSSE)
					if n == 1 {
						resp.Body = tracked
					}
					return resp, nil
				}, WithMaxConcurrent(1))
				r, err := c.CreateMessageStream(context.Background(), reliabilityRequest())
				if err != nil {
					t.Fatal(err)
				}
				queued := make(chan error, 1)
				go func() {
					next, err := c.CreateMessageStream(context.Background(), reliabilityRequest())
					if err == nil {
						err = next.Close()
					}
					queued <- err
				}()
				synctest.Wait()
				if calls.Load() != 1 || len(c.sem) != 1 {
					t.Fatal("stream did not retain permit")
				}
				if ending == "close" {
					if err := r.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					for {
						ev, err := r.Next()
						if err != nil {
							break
						}
						if ev.Type == EventMessageStop {
							break
						}
					}
				}
				if err := <-queued; err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for range 8 {
					wg.Go(func() {
						if err := r.Close(); err != nil {
							t.Error(err)
						}
					})
				}
				wg.Wait()
				if tracked.closes.Load() != 1 || len(c.sem) != 0 || calls.Load() != 2 {
					t.Fatalf("closes=%d permits=%d calls=%d", tracked.closes.Load(), len(c.sem), calls.Load())
				}
			})
		})
	}
}

func TestReliabilityCooldownAdmissionAndExtension(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClient("key", WithMaxConcurrent(1))
		c.sem <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		called := false
		done := make(chan error, 1)
		go func() { done <- c.doWithRetry(ctx, func(context.Context) error { called = true; return nil }, nil) }()
		synctest.Wait()
		c.setGlobalBackoff(time.Second)
		<-c.sem
		synctest.Wait()
		if called {
			t.Fatal("queued request bypassed cooldown")
		}
		c.setGlobalBackoff(3 * time.Second)
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if called {
			t.Fatal("waiter ignored cooldown extension")
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("error=%v", err)
		}
		if len(c.sem) != 0 {
			t.Fatal("canceled waiter leaked permit")
		}
	})
}

func TestReliabilityCooldownPublishedBeforeRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClient("key", WithMaxConcurrent(1))
		entered, unblock := make(chan struct{}), make(chan struct{})
		first := make(chan error, 1)
		no := false
		go func() {
			first <- c.doWithRetry(context.Background(), func(context.Context) error {
				close(entered)
				<-unblock
				return &RequestError{StatusCode: 429, retryAfter: 2, shouldRetry: &no}
			}, nil)
		}()
		<-entered
		start := time.Now()
		second := make(chan error, 1)
		go func() {
			second <- c.doWithRetry(context.Background(), func(context.Context) error {
				if time.Since(start) < 2*time.Second {
					t.Error("queued call bypassed published cooldown")
				}
				return nil
			}, nil)
		}()
		synctest.Wait()
		close(unblock)
		if err := <-first; err == nil {
			t.Fatal("expected rate limit error")
		}
		if err := <-second; err != nil {
			t.Fatal(err)
		}
	})
}

func TestReliabilityCancellationWhileQueued(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewClient("key", WithMaxConcurrent(1))
		c.sem <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- c.doWithRetry(ctx, func(context.Context) error { t.Error("canceled request sent"); return nil }, nil)
		}()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if len(c.sem) != 1 {
			t.Fatal("canceled waiter released another request's permit")
		}
		<-c.sem
	})
}

type reliabilityBlockingBody struct {
	ctx     context.Context
	reading chan struct{}
	closed  chan struct{}
	closes  atomic.Int32
}

func (b *reliabilityBlockingBody) Read([]byte) (int, error) {
	close(b.reading)
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}

func (b *reliabilityBlockingBody) Close() error {
	if b.closes.Add(1) == 1 {
		close(b.closed)
	}
	return nil
}

func TestReliabilityCancelOrCloseDuringRead(t *testing.T) {
	for _, cancelRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelRead), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				body := &reliabilityBlockingBody{ctx: ctx, reading: make(chan struct{}), closed: make(chan struct{})}
				c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
					resp := reliabilityResponse(r, 200, "")
					resp.Body = body
					return resp, nil
				}, WithMaxConcurrent(1))
				r, err := c.CreateMessageStream(ctx, reliabilityRequest())
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { _, err := r.Next(); done <- err }()
				<-body.reading
				wantErr := error(io.ErrClosedPipe)
				if cancelRead {
					cancel()
					wantErr = context.Canceled
				} else {
					if err := r.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := <-done; !errors.Is(err, wantErr) {
					t.Fatalf("error=%v, want %v", err, wantErr)
				}
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				if len(c.sem) != 0 || body.closes.Load() != 1 {
					t.Fatalf("permits=%d closes=%d", len(c.sem), body.closes.Load())
				}
			})
		})
	}
}

func TestReliabilityBlockingStopsBeforeHTTPBodyEOF(t *testing.T) {
	body := &reliabilityBody{Reader: io.MultiReader(strings.NewReader(reliabilityCompleteSSE), reliabilityNoEOF{})}
	c := reliabilityClient(func(r *http.Request) (*http.Response, error) {
		resp := reliabilityResponse(r, 200, "")
		resp.Body = body
		return resp, nil
	})
	resp, err := c.CreateMessage(context.Background(), reliabilityRequest())
	if err != nil || resp == nil || resp.ID != "msg_test" {
		t.Fatalf("response=%+v error=%v", resp, err)
	}
	if body.closes.Load() != 1 || len(c.sem) != 0 {
		t.Fatalf("closes=%d permits=%d", body.closes.Load(), len(c.sem))
	}
}
