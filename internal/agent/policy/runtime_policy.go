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

// Normalize applies compatibility-preserving defaults.
func (p RuntimePolicy) Normalize() RuntimePolicy {
	p.PermissionMode = NormalizePermissionMode(string(p.PermissionMode))
	if !p.EnableMCP && !p.EnableHooks && !p.EnablePlugins && len(p.AllowedAgentKinds) == 0 && len(p.AllowedSkills) == 0 {
		// Preserve the current default behavior unless the caller explicitly disables features.
		p.EnableMCP = true
	}
	return p
}
