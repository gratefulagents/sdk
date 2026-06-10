package agentsdk

import "testing"

// Unknown permission modes must fail closed to read-only — a typo such as
// "workspace-wrte" must not silently grant write access. Mirrors
// internal/agent/policy.NormalizePermissionMode.
func TestNormalizePermissionModeFailsClosed(t *testing.T) {
	tests := []struct {
		in   PermissionMode
		want PermissionMode
	}{
		{"", PermissionModeWorkspaceWrite}, // empty keeps the compatible default
		{"workspace-write", PermissionModeWorkspaceWrite},
		{"read-only", PermissionModeReadOnly},
		{"danger-full-access", PermissionModeDangerFullAccess},
		{"  Read-Only  ", PermissionModeReadOnly},
		{"workspace-wrte", PermissionModeReadOnly},     // typo fails closed
		{"full", PermissionModeReadOnly},               // unknown fails closed
		{"danger_full_access", PermissionModeReadOnly}, // wrong separator fails closed
	}
	for _, tt := range tests {
		if got := normalizePermissionMode(tt.in); got != tt.want {
			t.Errorf("normalizePermissionMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToolAccessLevelFromPermissionModeFailsClosed(t *testing.T) {
	if got := toolAccessLevelFromPermissionMode("workspace-wrte"); got != ToolAccessLevelReadOnly {
		t.Fatalf("unknown permission mode mapped to %q, want read-only tool access", got)
	}
	if got := toolAccessLevelFromPermissionMode(""); got != ToolAccessLevelFull {
		t.Fatalf("empty permission mode mapped to %q, want full (compatible default)", got)
	}
}
