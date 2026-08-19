package agent

import (
	"fmt"
	"strings"
)

// ModeReasoningLevel is an SDK-native reasoning effort label.
type ModeReasoningLevel string

const (
	ReasoningNone    ModeReasoningLevel = "none"
	ReasoningMinimal ModeReasoningLevel = "minimal"
	ReasoningLow     ModeReasoningLevel = "low"
	ReasoningMedium  ModeReasoningLevel = "medium"
	ReasoningHigh    ModeReasoningLevel = "high"
	ReasoningXHigh   ModeReasoningLevel = "xhigh"
	ReasoningMax     ModeReasoningLevel = "max"
)

// ModeTextVerbosity is an SDK-native text verbosity label.
type ModeTextVerbosity string

const (
	TextVerbosityLow    ModeTextVerbosity = "low"
	TextVerbosityMedium ModeTextVerbosity = "medium"
	TextVerbosityHigh   ModeTextVerbosity = "high"
)

// ModeModelRouting contains SDK-native model routing knobs.
type ModeModelRouting struct {
	DefaultModel   string
	FallbackModels []string
	ReasoningLevel string
	TextVerbosity  string
	RoleOverrides  map[string]ModeRoleModelRouting
}

// ModeRoleModelRouting contains routing overrides for one specialist role.
type ModeRoleModelRouting struct {
	Model          string
	FallbackModels []string
	ReasoningLevel string
	TextVerbosity  string
}

// ModeRoutingResolution is the effective top-level or per-role routing result
// after applying mode defaults, role overrides, and reasoning settings.
type ModeRoutingResolution struct {
	Model          string
	FallbackModels []string
	ReasoningLevel string
	TextVerbosity  string
	ModelSettings  ModelSettings
}

// ResolveModeRouting resolves the effective model and reasoning for the top-level
// agent from the mode's default routing.
func ResolveModeRouting(baseModel, provider string, routing *ModeModelRouting) ModeRoutingResolution {
	model := strings.TrimSpace(baseModel)
	var fallbacks []string
	reasoning := ModeReasoningLevel("")
	verbosity := ModeTextVerbosity("")

	if routing != nil {
		if override := strings.TrimSpace(routing.DefaultModel); override != "" {
			model = override
		}
		fallbacks = cloneNonEmptyStrings(routing.FallbackModels)
		reasoning = normalizeModeReasoningLevel(routing.ReasoningLevel)
		verbosity = normalizeModeTextVerbosity(routing.TextVerbosity)
	}

	if resolved := ResolveModelForProvider(model, provider); resolved != "" {
		model = resolved
	}
	fallbacks = resolveFallbackModelsForProvider(fallbacks, provider)

	return ModeRoutingResolution{
		Model:          model,
		FallbackModels: fallbacks,
		ReasoningLevel: string(reasoning),
		TextVerbosity:  string(verbosity),
		ModelSettings:  ModeRoutingSettings(reasoning, verbosity),
	}
}

// ResolveRoleModeRouting resolves the effective model and reasoning for a
// specialist role. Priority:
// 1. mode role override
// 2. mode defaults
func ResolveRoleModeRouting(
	baseModel, provider, roleName string,
	routing *ModeModelRouting,
) ModeRoutingResolution {
	resolved := ResolveModeRouting(baseModel, provider, routing)

	if routing != nil && routing.RoleOverrides != nil {
		if override, ok := routing.RoleOverrides[roleName]; ok {
			if model := strings.TrimSpace(override.Model); model != "" {
				resolved.Model = model
				if mapped := ResolveModelForProvider(model, provider); mapped != "" {
					resolved.Model = mapped
				}
			}
			if len(override.FallbackModels) > 0 {
				resolved.FallbackModels = resolveFallbackModelsForProvider(cloneNonEmptyStrings(override.FallbackModels), provider)
			}
			if level := normalizeModeReasoningLevel(override.ReasoningLevel); level != "" {
				resolved.ReasoningLevel = string(level)
				resolved.ModelSettings = resolved.ModelSettings.Merge(ModeReasoningSettings(level))
				if level == ReasoningNone {
					// Merge ignores zero values, so an explicit "none" must
					// clear any thinking budget inherited from mode defaults;
					// otherwise budget-driven providers keep thinking enabled.
					resolved.ModelSettings.ThinkingBudget = 0
				}
			}
			if verbosity := normalizeModeTextVerbosity(override.TextVerbosity); verbosity != "" {
				resolved.TextVerbosity = string(verbosity)
				resolved.ModelSettings = resolved.ModelSettings.Merge(ModeTextVerbositySettings(verbosity))
			}
		}
	}

	return resolved
}

func resolveFallbackModelsForProvider(models []string, provider string) []string {
	if len(models) == 0 {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if mapped := ResolveModelForProvider(model, provider); mapped != "" {
			model = mapped
		}
		out = append(out, model)
	}
	return out
}

// ModeReasoningSettings converts a reasoning level into concrete provider
// settings. The budgets stay below the default Anthropic max token limit so
// "high" reasoning still leaves room for the final answer.
func ModeReasoningSettings(level any) ModelSettings {
	switch normalizeModeReasoningLevel(level) {
	case ReasoningNone:
		// "none" disables reasoning. Models that accept effort "none"
		// (gpt-5.1+) receive it verbatim; older Responses models degrade to
		// minimal (the closest supported behavior). Budget-based providers
		// disable thinking entirely.
		return ModelSettings{ReasoningEffort: "none"}
	case ReasoningMinimal:
		// Anthropic's minimum accepted thinking budget is 1024.
		return ModelSettings{ThinkingBudget: 1024, ReasoningEffort: "minimal"}
	case ReasoningLow:
		return ModelSettings{ThinkingBudget: 2048, ReasoningEffort: "low"}
	case ReasoningMedium:
		return ModelSettings{ThinkingBudget: 4096, ReasoningEffort: "medium"}
	case ReasoningHigh:
		return ModelSettings{ThinkingBudget: 8192, ReasoningEffort: "high"}
	case ReasoningXHigh:
		// Budgets mirror reference harnesses (pi caps its top tiers at 16384)
		// while preserving OpenAI xhigh effort. Providers clamp the budget to
		// the request max-token limit at send time.
		return ModelSettings{ThinkingBudget: 16384, ReasoningEffort: "xhigh"}
	case ReasoningMax:
		// Max is a distinct OpenAI effort above xhigh; budget-based providers
		// get a correspondingly larger thinking budget so max is stronger
		// than xhigh there too.
		return ModelSettings{ThinkingBudget: 24576, ReasoningEffort: "max"}
	default:
		return ModelSettings{}
	}
}

// ModeTextVerbositySettings converts a verbosity level into concrete
// OpenAI Responses text.verbosity settings.
func ModeTextVerbositySettings(level any) ModelSettings {
	switch normalizeModeTextVerbosity(level) {
	case TextVerbosityLow:
		return ModelSettings{TextVerbosity: "low"}
	case TextVerbosityMedium:
		return ModelSettings{TextVerbosity: "medium"}
	case TextVerbosityHigh:
		return ModelSettings{TextVerbosity: "high"}
	default:
		return ModelSettings{}
	}
}

// ModeRoutingSettings converts mode routing behavior knobs into provider
// settings for a single model request.
func ModeRoutingSettings(reasoning any, verbosity any) ModelSettings {
	return ModeReasoningSettings(reasoning).Merge(ModeTextVerbositySettings(verbosity))
}

func normalizeModeReasoningLevel(level any) ModeReasoningLevel {
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(level))) {
	case string(ReasoningNone):
		return ReasoningNone
	case string(ReasoningMinimal):
		return ReasoningMinimal
	case string(ReasoningLow):
		return ReasoningLow
	case string(ReasoningMedium):
		return ReasoningMedium
	case string(ReasoningHigh):
		return ReasoningHigh
	case string(ReasoningXHigh):
		return ReasoningXHigh
	case string(ReasoningMax):
		return ReasoningMax
	default:
		return ""
	}
}

func normalizeModeTextVerbosity(level any) ModeTextVerbosity {
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(level))) {
	case string(TextVerbosityLow):
		return TextVerbosityLow
	case string(TextVerbosityMedium):
		return TextVerbosityMedium
	case string(TextVerbosityHigh):
		return TextVerbosityHigh
	default:
		return ""
	}
}
