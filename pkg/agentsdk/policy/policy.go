package policy

import internalpolicy "github.com/gratefulagents/sdk/internal/agent/policy"

type PermissionMode = internalpolicy.PermissionMode
type GitRemoteWrites = internalpolicy.GitRemoteWrites
type RuntimePolicy = internalpolicy.RuntimePolicy
type ToolPolicy = internalpolicy.ToolPolicy

const (
	PermissionModeReadOnly         = internalpolicy.PermissionModeReadOnly
	PermissionModeWorkspaceWrite   = internalpolicy.PermissionModeWorkspaceWrite
	PermissionModeDangerFullAccess = internalpolicy.PermissionModeDangerFullAccess
	GitRemoteWritesEnabled         = internalpolicy.GitRemoteWritesEnabled
	GitRemoteWritesDisabled        = internalpolicy.GitRemoteWritesDisabled
)

var (
	NormalizePermissionMode  = internalpolicy.NormalizePermissionMode
	NormalizeGitRemoteWrites = internalpolicy.NormalizeGitRemoteWrites
	NewToolPolicy            = internalpolicy.NewToolPolicy
)
