package anthropic

import (
	"strconv"
	"strings"
)

// Claude exposes two mutually incompatible extended-thinking request shapes,
// split by model generation:
//
//   - thinking.type=enabled + budget_tokens: the classic shape, implemented by
//     the 4.5-and-older generations (claude-haiku-4.5, claude-sonnet-4.5,
//     claude-opus-4.5, claude-sonnet-4, claude-3-x). Effort-only models reject
//     it (live 400: `"thinking.type.enabled" is not supported for this model.
//     Use "thinking.type.adaptive" and "output_config.effort" ...`).
//   - thinking.type=adaptive + output_config.effort: the effort-first shape
//     introduced with the 4.6 generation (claude-sonnet-4.6, claude-opus-4.6+,
//     claude-sonnet-5, claude-fable-5). Older models reject or ignore it —
//     notably GitHub Copilot's /v1/messages shim returns no thinking blocks at
//     all for adaptive requests against the 4.5 family, silently disabling
//     visible reasoning.
//
// The split below mirrors the models.dev reasoning_options catalog for the
// "anthropic" and "github-copilot" providers (and opencode's per-model
// adaptive-thinking gating). Callers that guess wrong self-heal via the
// thinking-shape 400 retry in the providers layer.

// ModelSupportsAdaptiveThinking reports whether a Claude model accepts
// thinking.type=adaptive + output_config.effort: the fable family and every
// sonnet/opus/haiku generation from 4.6 upward.
func ModelSupportsAdaptiveThinking(model string) bool {
	family, major, minor, ok := claudeThinkingGeneration(model)
	if !ok {
		return false
	}
	if family == "fable" {
		return true
	}
	return major > 4 || (major == 4 && minor >= 6)
}

// ModelRequiresAdaptiveThinking reports whether a Claude model accepts ONLY
// the adaptive shape on the first-party Messages API, i.e. rejects
// thinking.type=enabled + budget_tokens: the fable family, opus 4.7+, and any
// generation 5 model. (claude-sonnet-4.6 and claude-opus-4.6 still accept
// budget_tokens on api.anthropic.com, so they are not in this set.)
func ModelRequiresAdaptiveThinking(model string) bool {
	family, major, minor, ok := claudeThinkingGeneration(model)
	if !ok {
		return false
	}
	if family == "fable" {
		return true
	}
	if major >= 5 {
		return true
	}
	return family == "opus" && major == 4 && minor >= 7
}

// claudeThinkingGeneration parses a Claude model identifier into its family
// and version. It tolerates the dotted (claude-sonnet-4.6), dashed
// (claude-sonnet-4-6), dated (claude-sonnet-4-5-20250929), and legacy
// version-first (claude-3-7-sonnet) naming forms; date stamps (>= 1000) are
// not version segments. ok is false for non-Claude models.
func claudeThinkingGeneration(model string) (family string, major, minor int, ok bool) {
	normalized := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(normalized, "/"); idx >= 0 {
		normalized = normalized[idx+1:]
	}
	if !strings.Contains(normalized, "claude") {
		return "", 0, 0, false
	}

	versionSeen := false
	for _, token := range strings.Split(normalized, "-") {
		switch token {
		case "fable", "sonnet", "opus", "haiku":
			if family == "" {
				family = token
			}
			continue
		}
		if versionSeen && minor > 0 {
			continue
		}
		// A token is a version segment when it is numeric ("4", "5") or
		// dotted-numeric ("4.6"); 4+ digit numbers are date stamps.
		for _, part := range strings.SplitN(token, ".", 2) {
			n, err := strconv.Atoi(part)
			if err != nil || n >= 1000 {
				break
			}
			if !versionSeen {
				major = n
				versionSeen = true
			} else if minor == 0 {
				minor = n
			}
		}
	}
	if family == "" && !versionSeen {
		return "", 0, 0, false
	}
	return family, major, minor, true
}
