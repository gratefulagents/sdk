package policy

import "testing"

func TestNormalizePermissionMode(t *testing.T) {
	tests := []struct {
		input string
		want  PermissionMode
	}{
		{"", PermissionModeWorkspaceWrite},
		{"read-only", PermissionModeReadOnly},
		{"workspace-write", PermissionModeWorkspaceWrite},
		{"danger-full-access", PermissionModeDangerFullAccess},
		{"unexpected", PermissionModeReadOnly},
	}

	for _, tt := range tests {
		if got := NormalizePermissionMode(tt.input); got != tt.want {
			t.Errorf("NormalizePermissionMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPermissionModeAllowsMCPTool(t *testing.T) {
	if PermissionModeReadOnly.AllowsMCPTool(false) {
		t.Fatal("read-only mode should block write-capable MCP tools")
	}
	if !PermissionModeReadOnly.AllowsMCPTool(true) {
		t.Fatal("read-only mode should allow read-only MCP tools")
	}
	if !PermissionModeWorkspaceWrite.AllowsMCPTool(false) {
		t.Fatal("workspace-write should allow MCP tools")
	}
}
