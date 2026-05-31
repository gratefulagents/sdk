package agents

import "testing"

func TestBuiltInKindsAreStable(t *testing.T) {
	kinds := BuiltInKinds()
	if len(kinds) == 0 {
		t.Fatal("expected built-in kinds")
	}

	for _, kind := range kinds {
		if kind == "" {
			t.Fatal("expected non-empty built-in kind")
		}
	}
}

func TestResolveKindSupportsLegacyAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  Kind
	}{
		{"explore", KindExplore},
		{"Explore", KindExplore},
		{"Plan", KindPlanner},
		{"security-reviewer", KindSecurityReviewer},
		{"general-purpose", KindExecutor},
	}

	for _, tt := range tests {
		def, ok := ResolveKind(tt.input)
		if !ok {
			t.Fatalf("ResolveKind(%q) failed", tt.input)
		}
		if def.Kind != tt.want {
			t.Fatalf("ResolveKind(%q) = %q, want %q", tt.input, def.Kind, tt.want)
		}
	}
}

func TestResolveKindUnknown(t *testing.T) {
	t.Parallel()

	if _, ok := ResolveKind("unknown-role"); ok {
		t.Fatal("ResolveKind should reject unknown kinds")
	}
}

func TestIsAllowedResolvesAliases(t *testing.T) {
	t.Parallel()

	if !IsAllowed(KindExecutor, []string{"explore", "general-purpose"}) {
		t.Fatal("expected executor to be allowed by legacy alias")
	}
	if IsAllowed(KindExecutor, []string{"explore", "security-reviewer"}) {
		t.Fatal("did not expect executor to be allowed")
	}
}
