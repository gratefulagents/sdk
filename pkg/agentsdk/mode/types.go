// Package mode defines the SDK-native mode template shape.
//
// A mode template is a named runtime preset for the parent agent: prompt
// instructions, model routing, run constraints, autonomy, and tool access.
// Specialist sub-agents are configured separately through the role catalog.
package mode

// TemplateSpec is the SDK-native mode template shape.
type TemplateSpec struct {
	Name        string
	Version     string
	DisplayName string
	Description string
	Category    string
	Autonomous  bool
	// ToolAccess constrains the run's tool surface while the mode is active.
	// Recognized values: "" (inherit), "full", "read-only".
	ToolAccess   string
	Instructions string
	ModelRouting *ModelRouting
	Constraints  *Constraints
}

type Constraints struct {
	MaxTurns               int
	SubAgentMaxTurns       int
	MaxConcurrentSubAgents int
	MaxRetries             int
	MaxRuntimeMinutes      int
}

type ModelRouting struct {
	DefaultModel   string
	FallbackModels []string
	ReasoningLevel string
	TextVerbosity  string
	RoleOverrides  map[string]RoleModelRouting
}

type RoleModelRouting struct {
	Model          string
	FallbackModels []string
	ReasoningLevel string
	TextVerbosity  string
}
