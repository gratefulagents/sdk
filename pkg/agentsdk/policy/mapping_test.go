package policy

import (
	"testing"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

func TestToolAccessLevelFromPermissionMode(t *testing.T) {
	tests := []struct {
		mode PermissionMode
		want agentsdk.ToolAccessLevel
	}{
		{PermissionModeReadOnly, agentsdk.ToolAccessLevelReadOnly},
		{PermissionModeWorkspaceWrite, agentsdk.ToolAccessLevelFull},
		{PermissionModeDangerFullAccess, agentsdk.ToolAccessLevelFull},
	}
	for _, tt := range tests {
		if got := ToolAccessLevelFromPermissionMode(tt.mode); got != tt.want {
			t.Fatalf("ToolAccessLevelFromPermissionMode(%q) = %q, want %q", tt.mode, got, tt.want)
		}
	}
}

func TestToolPolicyFromPermissionMode(t *testing.T) {
	if got := ToolPolicyFromPermissionMode(PermissionModeDangerFullAccess); got != nil {
		t.Fatalf("danger-full-access policy = %#v, want nil", got)
	}
	if got := ToolPolicyFromPermissionMode(PermissionModeWorkspaceWrite); got == nil || !got.ApprovalRequired {
		t.Fatalf("workspace-write policy = %#v, want approvals required", got)
	}
	if got := ToolPolicyFromPermissionMode(PermissionModeReadOnly); got == nil || !got.ApprovalRequired {
		t.Fatalf("read-only policy = %#v, want approvals required", got)
	}
}
