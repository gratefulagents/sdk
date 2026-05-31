package agent

import (
	"math"
	"time"
)

// RetryPolicy configures automatic retry behavior for model calls.
type RetryPolicy struct {
	MaxRetries int
	Backoff    RetryBackoffSettings
}

// RetryBackoffSettings controls exponential backoff.
type RetryBackoffSettings struct {
	InitialDelayMS int
	MaxDelayMS     int
	Multiplier     float64
}

// DefaultRetryPolicy returns a sensible default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries: 3,
		Backoff: RetryBackoffSettings{
			InitialDelayMS: 1000,
			MaxDelayMS:     30000,
			Multiplier:     2.0,
		},
	}
}

// DelayForAttempt calculates the backoff delay for a given retry attempt (0-indexed).
func (p *RetryPolicy) DelayForAttempt(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Duration(p.Backoff.InitialDelayMS) * time.Millisecond
	}
	delay := float64(p.Backoff.InitialDelayMS) * math.Pow(p.Backoff.Multiplier, float64(attempt))
	if delay > float64(p.Backoff.MaxDelayMS) {
		delay = float64(p.Backoff.MaxDelayMS)
	}
	return time.Duration(delay) * time.Millisecond
}
