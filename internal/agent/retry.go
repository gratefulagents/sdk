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

// maxAdviceRetriesPerTurn bounds provider-advised retries (advice.ShouldRetry)
// for a single turn, counting all failed attempts of that turn. Without a
// bound, a persistently rate-limited provider would be retried until MaxTurns,
// burning the whole turn budget on one request.
const maxAdviceRetriesPerTurn = 10

// adviceRetryDelay returns the backoff floor for a provider-advised retry that
// carries no provider-directed delay. It follows the run's retry policy curve
// (attempt is 1-indexed) so advised retries after the policy budget keep
// growing instead of hammering with zero delay.
func adviceRetryDelay(policy *RetryPolicy, attempt int) time.Duration {
	p := DefaultRetryPolicy()
	if policy != nil && policy.Backoff.InitialDelayMS > 0 {
		p = *policy
	}
	if attempt < 1 {
		attempt = 1
	}
	return p.DelayForAttempt(attempt - 1)
}
