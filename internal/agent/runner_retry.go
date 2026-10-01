package agent

import "time"

func modelRetryDelay(policy *RetryPolicy, advice *ModelRetryAdvice, attempt int) (time.Duration, bool) {
	var delayMS int64
	switch {
	case shouldRetryWithPolicy(policy, attempt) && !retryPolicyBlockedByAdvice(advice):
		delayMS = policy.DelayForAttempt(attempt - 1).Milliseconds()
		// Never retry earlier than the provider's Retry-After, even when the
		// configured policy would otherwise retry immediately.
		if advice != nil && advice.RetryAfterMS > delayMS {
			delayMS = advice.RetryAfterMS
		}
	case advice != nil && advice.ShouldRetry && attempt <= maxAdviceRetriesPerTurn:
		delayMS = advice.RetryAfterMS
		if delayMS <= 0 {
			delayMS = adviceRetryDelay(policy, attempt).Milliseconds()
		}
	default:
		return 0, false
	}
	return time.Duration(capRetryAfterMS(delayMS)) * time.Millisecond, true
}
