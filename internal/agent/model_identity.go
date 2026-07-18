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
		if p != "" {
			// Raw already carries a routing prefix: keep it as the canonical
			// identity (lowercased for comparability) instead of fabricating
			// one from the wire-protocol provider name.
			prefix := strings.ToLower(p)
			if provider == "" {
				provider = prefix
			}
			canonical = prefix + "/" + bare
		} else if provider != "" {
			canonical = provider + "/" + bare
		}
	}
	return ModelIdentity{Raw: trimmed, Provider: provider, Canonical: canonical}
}
