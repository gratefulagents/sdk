package policy

// RuntimePolicy is the resolved runtime surface exposed to an agent session.
type RuntimePolicy struct {
	PermissionMode    PermissionMode
	EnableMCP         bool
	EnableHooks       bool
	EnablePlugins     bool
	AllowedAgentKinds []string
	AllowedSkills     []string
}

// Normalize applies fail-closed defaults. A zero-value policy enables no
// optional runtime features.
func (p RuntimePolicy) Normalize() RuntimePolicy {
	p.PermissionMode = NormalizePermissionMode(string(p.PermissionMode))
	return p
}
