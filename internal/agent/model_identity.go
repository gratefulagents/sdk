package agent

import "strings"

type ModelIdentity struct {
	Raw       string
	Provider  string
	Canonical string
}

func NormalizeModelIdentity(raw string, provider string) ModelIdentity {
	trimmed := strings.TrimSpace(raw)
	provider = strings.ToLower(strings.TrimSpace(provider))
	canonical := trimmed
	if p, bare := ParseModelPrefix(trimmed); bare != "" {
		if provider == "" {
			provider = strings.ToLower(strings.TrimSpace(p))
		}
		canonical = trimmed
		if provider != "" && !strings.HasPrefix(strings.ToLower(trimmed), provider+"/") {
			canonical = provider + "/" + bare
		}
	}
	return ModelIdentity{Raw: trimmed, Provider: provider, Canonical: canonical}
}
