package policy

import "strings"

// PermissionMode controls the mutability of the runtime tool surface.
type PermissionMode string

const (
	PermissionModeReadOnly         PermissionMode = "read-only"
	PermissionModeWorkspaceWrite   PermissionMode = "workspace-write"
	PermissionModeDangerFullAccess PermissionMode = "danger-full-access"
)

// NormalizePermissionMode resolves the permission mode string to a known value.
// An empty value selects the compatible default (workspace-write); any other
// unrecognized value fails closed to read-only to avoid silently granting write
// access on a misconfiguration or typo.
func NormalizePermissionMode(mode string) PermissionMode {
	switch PermissionMode(strings.TrimSpace(mode)) {
	case PermissionModeReadOnly:
		return PermissionModeReadOnly
	case PermissionModeDangerFullAccess:
		return PermissionModeDangerFullAccess
	case PermissionModeWorkspaceWrite, "":
		return PermissionModeWorkspaceWrite
	default:
		return PermissionModeReadOnly
	}
}

func (m PermissionMode) AllowsWriteTools() bool {
	return NormalizePermissionMode(string(m)) != PermissionModeReadOnly
}

func (m PermissionMode) AllowsMCPTool(readOnlyHint bool) bool {
	if NormalizePermissionMode(string(m)) == PermissionModeReadOnly {
		return readOnlyHint
	}
	return true
}
