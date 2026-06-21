package agent

import (
	"errors"
	"strconv"
	"strings"
)

func effectiveFallbackModels(a *Agent, cfg RunConfig) []string {
	if len(cfg.FallbackModels) > 0 {
		return cloneNonEmptyStrings(cfg.FallbackModels)
	}
	if a == nil {
		return nil
	}
	return cloneNonEmptyStrings(a.FallbackModels)
}

func nextFallbackModel(primary, current string, fallbacks []string) (string, bool) {
	chain := fallbackModelChain(primary, fallbacks)
	if len(chain) <= 1 {
		return "", false
	}
	current = strings.TrimSpace(current)
	idx := 0
	for i, model := range chain {
		if sameModelID(model, current) {
			idx = i
			break
		}
	}
	for _, model := range chain[idx+1:] {
		if strings.TrimSpace(model) != "" {
			return model, true
		}
	}
	return "", false
}

func fallbackModelChain(primary string, fallbacks []string) []string {
	chain := make([]string, 0, len(fallbacks)+1)
	seen := map[string]struct{}{}
	appendModel := func(model string) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		chain = append(chain, model)
	}
	appendModel(primary)
	for _, model := range fallbacks {
		appendModel(model)
	}
	return chain
}

func cloneNonEmptyStrings(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}

func shouldFallbackModelCall(err error, advice *ModelRetryAdvice) bool {
	if err == nil || isContextCancellation(err) || isContextLengthExceededError(err) {
		return false
	}
	var behaviorErr *ModelBehaviorError
	if errors.As(err, &behaviorErr) {
		return false
	}
	var committedErr *streamOutputCommittedError
	if errors.As(err, &committedErr) {
		return false
	}
	if advice == nil || !advice.ShouldRetry {
		return false
	}
	return fallbackReason(advice) != ""
}

func fallbackReason(advice *ModelRetryAdvice) string {
	if advice == nil {
		return ""
	}
	reason := strings.ToLower(strings.TrimSpace(advice.Reason))
	if reason == "" {
		return ""
	}
	if code, err := strconv.Atoi(reason); err == nil {
		if code == 429 {
			return "rate_limit"
		}
		return ""
	}
	switch {
	case strings.Contains(reason, "rate_limit"), strings.Contains(reason, "too_many_requests"):
		return "rate_limit"
	case strings.Contains(reason, "quota"), strings.Contains(reason, "billing"), strings.Contains(reason, "subscription"):
		return "quota"
	case strings.Contains(reason, "limit_exceeded"), strings.Contains(reason, "exhausted"):
		return "quota"
	default:
		return ""
	}
}

func sameModelID(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

type streamOutputCommittedError struct {
	cause error
}

func (e *streamOutputCommittedError) Error() string {
	if e == nil || e.cause == nil {
		return "model stream failed after output was emitted"
	}
	return e.cause.Error()
}

func (e *streamOutputCommittedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}
