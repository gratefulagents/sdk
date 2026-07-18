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
// Partially-populated policies are normalized: an unset Multiplier or
// MaxDelayMS would otherwise collapse every later delay to 0 ms and hot-loop
// retries against a failing provider. An explicit InitialDelayMS <= 0 keeps
// meaning "retry immediately".
func (p *RetryPolicy) DelayForAttempt(attempt int) time.Duration {
	initial := p.Backoff.InitialDelayMS
	if initial <= 0 {
		return 0
	}
	if attempt <= 0 {
		return time.Duration(initial) * time.Millisecond
	}
	multiplier := p.Backoff.Multiplier
	if multiplier < 1 {
		multiplier = 2.0
	}
	maxDelayMS := p.Backoff.MaxDelayMS
	if maxDelayMS <= 0 {
		maxDelayMS = 30000
	}
	delay := float64(initial) * math.Pow(multiplier, float64(attempt))
	if delay > float64(maxDelayMS) {
		delay = float64(maxDelayMS)
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
