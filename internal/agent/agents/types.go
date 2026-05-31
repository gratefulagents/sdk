package agents

import "strings"

// Kind is the governed catalog of built-in agent roles.
type Kind string

const (
	KindExplore          Kind = "explore"
	KindAnalyst          Kind = "analyst"
	KindPlanner          Kind = "planner"
	KindArchitect        Kind = "architect"
	KindDebugger         Kind = "debugger"
	KindExecutor         Kind = "executor"
	KindCritic           Kind = "critic"
	KindSecurityReviewer Kind = "security-reviewer"
	KindDependencyExpert Kind = "dependency-expert"
	KindDesigner         Kind = "designer"
	KindWriter           Kind = "writer"
)

type Definition struct {
	Kind     Kind     `json:"kind"`
	ReadOnly bool     `json:"readOnly"`
	Aliases  []string `json:"aliases,omitempty"`
}

var builtInDefinitions = []Definition{
	{Kind: KindExplore, ReadOnly: true, Aliases: []string{"Explore"}},
	{Kind: KindAnalyst},
	{Kind: KindPlanner, ReadOnly: true, Aliases: []string{"Plan"}},
	{Kind: KindArchitect},
	{Kind: KindDebugger},
	{Kind: KindExecutor, Aliases: []string{"general-purpose"}},
	{Kind: KindCritic},
	{Kind: KindSecurityReviewer, ReadOnly: true},
	{Kind: KindDependencyExpert},
	{Kind: KindDesigner},
	{Kind: KindWriter},
}

func BuiltInKinds() []Kind {
	out := make([]Kind, 0, len(builtInDefinitions))
	for _, def := range builtInDefinitions {
		out = append(out, def.Kind)
	}
	return out
}

func Definitions() []Definition {
	out := make([]Definition, len(builtInDefinitions))
	copy(out, builtInDefinitions)
	return out
}

func ParseKind(v string) (Kind, bool) {
	def, ok := ResolveKind(v)
	if !ok {
		return "", false
	}
	return def.Kind, true
}

func ResolveKind(v string) (Definition, bool) {
	normalized := normalizeKindValue(v)
	if normalized == "" {
		return Definition{}, false
	}
	for _, def := range builtInDefinitions {
		if normalizeKindValue(string(def.Kind)) == normalized {
			return def, true
		}
		for _, alias := range def.Aliases {
			if normalizeKindValue(alias) == normalized {
				return def, true
			}
		}
	}
	return Definition{}, false
}

func IsAllowed(kind Kind, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		def, ok := ResolveKind(candidate)
		if ok && def.Kind == kind {
			return true
		}
	}
	return false
}

func normalizeKindValue(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}
