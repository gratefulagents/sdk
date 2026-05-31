package policy

// ToolPolicy exposes the tool-specific permissions derived from RuntimePolicy.
type ToolPolicy struct {
	PermissionMode PermissionMode
}

func NewToolPolicy(runtimePolicy RuntimePolicy) ToolPolicy {
	normalized := runtimePolicy.Normalize()
	return ToolPolicy{
		PermissionMode: normalized.PermissionMode,
	}
}

func (p ToolPolicy) AllowsWriteTools() bool {
	return p.PermissionMode.AllowsWriteTools()
}
