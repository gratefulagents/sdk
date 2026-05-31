package policy

import internalpolicy "github.com/gratefulagents/sdk/internal/agent/policy"

type PermissionMode = internalpolicy.PermissionMode
type RuntimePolicy = internalpolicy.RuntimePolicy
type ToolPolicy = internalpolicy.ToolPolicy

const (
	PermissionModeReadOnly         = internalpolicy.PermissionModeReadOnly
	PermissionModeWorkspaceWrite   = internalpolicy.PermissionModeWorkspaceWrite
	PermissionModeDangerFullAccess = internalpolicy.PermissionModeDangerFullAccess
)

var (
	NormalizePermissionMode = internalpolicy.NormalizePermissionMode
	NewToolPolicy           = internalpolicy.NewToolPolicy
)
