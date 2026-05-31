package mcp

import (
	"strings"
	"testing"
)

func TestNormalizeNameForMCP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "keeps safe chars", in: "github-server_1", want: "github-server_1"},
		{name: "replaces spaces", in: "github tools", want: "github_tools"},
		{name: "trims noise", in: "  / weird /  ", want: "weird"},
		{name: "empty fallback", in: "   ", want: "unnamed"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeNameForMCP(tc.in)
			if got != tc.want {
				t.Fatalf("NormalizeNameForMCP(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildToolName_LengthAndPrefix(t *testing.T) {
	t.Parallel()

	name := BuildToolName(strings.Repeat("server", 8), strings.Repeat("tool", 16))
	if len(name) > maxMCPToolNameLen {
		t.Fatalf("tool name length = %d, max %d (%q)", len(name), maxMCPToolNameLen, name)
	}
	if !strings.HasPrefix(name, "mcp__") {
		t.Fatalf("tool name %q should start with mcp__", name)
	}
}

func TestEnsureUniqueToolName(t *testing.T) {
	t.Parallel()

	used := map[string]struct{}{}
	first := EnsureUniqueToolName("mcp__a__b", used)
	second := EnsureUniqueToolName("mcp__a__b", used)
	third := EnsureUniqueToolName("mcp__a__b", used)

	if first != "mcp__a__b" {
		t.Fatalf("first = %q, want base name", first)
	}
	if second == first {
		t.Fatalf("second should differ from first")
	}
	if third == second {
		t.Fatalf("third should differ from second")
	}
}
