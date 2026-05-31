package anthropic

import (
	"testing"
)

func TestNewClient_Defaults(t *testing.T) {
	c := NewClient("test-key")
	if c == nil {
		t.Fatalf("NewClient returned nil")
	}
	if c.sem == nil {
		t.Fatalf("NewClient did not initialize semaphore")
	}
}

func TestRequestError_Retryable(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{529, true},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			err := &RequestError{StatusCode: tt.code}
			if got := err.Retryable(); got != tt.want {
				t.Errorf("Retryable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestErrorRetryAfterMSCapsHugeProviderDelay(t *testing.T) {
	err := &RequestError{retryAfter: maxRetryAfterSeconds + 3600}
	if got, want := err.RetryAfterMS(), maxRetryAfterSeconds*1000; got != want {
		t.Fatalf("RetryAfterMS() = %d, want %d", got, want)
	}
}
