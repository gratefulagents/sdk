package policy

import "github.com/gratefulagents/sdk/pkg/agentsdk"

// ToolPolicyFromPermissionMode maps a host permission mode to the SDK run
// ToolPolicy used by the runner.
func ToolPolicyFromPermissionMode(mode PermissionMode) *agentsdk.ToolPolicy {
	switch NormalizePermissionMode(string(mode)) {
	case PermissionModeDangerFullAccess:
		return nil
	default:
		return &agentsdk.ToolPolicy{ApprovalRequired: true}
	}
}

// ToolAccessLevelFromPermissionMode maps a host permission mode to the SDK
// access tier used for filtering the available tool surface.
func ToolAccessLevelFromPermissionMode(mode PermissionMode) agentsdk.ToolAccessLevel {
	if NormalizePermissionMode(string(mode)) == PermissionModeReadOnly {
		return agentsdk.ToolAccessLevelReadOnly
	}
	return agentsdk.ToolAccessLevelFull
}
