package mode

// AgentCatalog contains built-in OMX agent role metadata.
var AgentCatalog = map[string]AgentCapability{
	"explore":               {Name: "explore", Description: "Fast codebase search and file/symbol mapping", Category: "build", ToolAccess: "read-only", DefaultModel: "mini", RoutingRole: RoutingRoleSpecialist},
	"analyst":               {Name: "analyst", Description: "Requirements clarity, acceptance criteria, hidden constraints", Category: "build", ToolAccess: "analysis", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"planner":               {Name: "planner", Description: "Task sequencing, execution plans, risk flags", Category: "build", ToolAccess: "analysis", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"architect":             {Name: "architect", Description: "System design, boundaries, interfaces, long-horizon tradeoffs", Category: "build", ToolAccess: "read-only", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"debugger":              {Name: "debugger", Description: "Root-cause analysis, regression isolation, failure diagnosis", Category: "build", ToolAccess: "analysis", DefaultModel: "powerful", RoutingRole: RoutingRoleExecutor},
	"executor":              {Name: "executor", Description: "Code implementation, refactoring, feature work", Category: "build", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleExecutor},
	"team-executor":         {Name: "team-executor", Description: "Supervised team execution for conservative delivery lanes", Category: "build", ToolAccess: "execution", DefaultModel: "powerful", RoutingRole: RoutingRoleExecutor},
	"style-reviewer":        {Name: "style-reviewer", Description: "Formatting, naming, idioms, lint conventions", Category: "review", ToolAccess: "read-only", DefaultModel: "mini", RoutingRole: RoutingRoleSpecialist},
	"quality-reviewer":      {Name: "quality-reviewer", Description: "Logic defects, maintainability, anti-patterns", Category: "review", ToolAccess: "read-only", DefaultModel: "standard", RoutingRole: RoutingRoleLeader},
	"api-reviewer":          {Name: "api-reviewer", Description: "API contracts, versioning, backward compatibility", Category: "review", ToolAccess: "read-only", DefaultModel: "standard", RoutingRole: RoutingRoleLeader},
	"security-reviewer":     {Name: "security-reviewer", Description: "Vulnerabilities, trust boundaries, authn/authz", Category: "review", ToolAccess: "read-only", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"performance-reviewer":  {Name: "performance-reviewer", Description: "Hotspots, complexity, memory/latency optimization", Category: "review", ToolAccess: "read-only", DefaultModel: "standard", RoutingRole: RoutingRoleLeader},
	"code-reviewer":         {Name: "code-reviewer", Description: "Comprehensive review across all concerns", Category: "review", ToolAccess: "read-only", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"dependency-expert":     {Name: "dependency-expert", Description: "External SDK/API/package evaluation", Category: "domain", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"test-engineer":         {Name: "test-engineer", Description: "Test strategy, coverage, flaky-test hardening", Category: "domain", ToolAccess: "execution", DefaultModel: "powerful", RoutingRole: RoutingRoleExecutor},
	"quality-strategist":    {Name: "quality-strategist", Description: "Quality strategy, release readiness, risk assessment", Category: "domain", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleLeader},
	"build-fixer":           {Name: "build-fixer", Description: "Build/toolchain/type failures resolution", Category: "domain", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleExecutor},
	"designer":              {Name: "designer", Description: "UX/UI architecture, interaction design", Category: "domain", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleExecutor},
	"writer":                {Name: "writer", Description: "Documentation, migration notes, user guidance", Category: "domain", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"qa-tester":             {Name: "qa-tester", Description: "Interactive CLI/service runtime validation", Category: "domain", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleExecutor},
	"git-master":            {Name: "git-master", Description: "Commit strategy, history hygiene, rebasing", Category: "domain", ToolAccess: "execution", DefaultModel: "standard", RoutingRole: RoutingRoleExecutor},
	"code-simplifier":       {Name: "code-simplifier", Description: "Simplifies recently modified code for clarity and consistency without changing behavior", Category: "domain", ToolAccess: "execution", DefaultModel: "powerful", RoutingRole: RoutingRoleExecutor},
	"researcher":            {Name: "researcher", Description: "External documentation and reference research", Category: "domain", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"product-manager":       {Name: "product-manager", Description: "Problem framing, personas/JTBD, PRDs", Category: "product", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleLeader},
	"ux-researcher":         {Name: "ux-researcher", Description: "Heuristic audits, usability, accessibility", Category: "product", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"information-architect": {Name: "information-architect", Description: "Taxonomy, navigation, findability", Category: "product", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"product-analyst":       {Name: "product-analyst", Description: "Product metrics, funnel analysis, experiments", Category: "product", ToolAccess: "analysis", DefaultModel: "standard", RoutingRole: RoutingRoleSpecialist},
	"critic":                {Name: "critic", Description: "Plan/design critical challenge and review", Category: "coordination", ToolAccess: "read-only", DefaultModel: "powerful", RoutingRole: RoutingRoleLeader},
	"vision":                {Name: "vision", Description: "Image/screenshot/diagram analysis", Category: "coordination", ToolAccess: "read-only", DefaultModel: "powerful", RoutingRole: RoutingRoleSpecialist},
}

// ResolveCapabilities maps names to catalog entries. Unknown names receive defaults.
func ResolveCapabilities(names []string) []AgentCapability {
	caps := make([]AgentCapability, 0, len(names))
	for _, name := range names {
		if cap, ok := AgentCatalog[name]; ok {
			caps = append(caps, cap)
		} else {
			caps = append(caps, AgentCapability{
				Name:         name,
				Category:     "unknown",
				ToolAccess:   "full",
				DefaultModel: "standard",
				Description:  "Custom capability: " + name,
			})
		}
	}
	return caps
}
