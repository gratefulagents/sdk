package agent

import "testing"

func TestResolveModelForProviderLeavesUnknownProvidersAlone(t *testing.T) {
	got := ResolveModelForProvider("small", "local")
	if got != "small" {
		t.Fatalf("ResolveModelForProvider() = %q, want small", got)
	}
}
