package mode

// RoutingRole determines how an agent participates in team coordination.
type RoutingRole string

const (
	RoutingRoleLeader     RoutingRole = "leader"
	RoutingRoleSpecialist RoutingRole = "specialist"
	RoutingRoleExecutor   RoutingRole = "executor"
)

// AgentCapability describes a specialized agent role that can be required by mode lanes.
type AgentCapability struct {
	Name         string
	Description  string
	Category     string
	ToolAccess   string
	DefaultModel string
	RoutingRole  RoutingRole
}

// TemplateSpec is the SDK-native mode template shape.
type TemplateSpec struct {
	Name             string
	Version          string
	DisplayName      string
	Description      string
	Category         string
	Autonomous       bool
	Instructions     string
	Phases           []Phase
	Transitions      []Transition
	Capabilities     []string
	ModelRouting     *ModelRouting
	RoleInstructions map[string]string
	Constraints      *Constraints
	ResetTo          *ResetTo
}

type Phase struct {
	ID               string
	Name             string
	Description      string
	ReadOnly         bool
	RequiresApproval bool
	PresentArtifact  string
	EntryGates       []PhaseGate
}

type PhaseGate struct {
	Require string
	Message string
}

type Transition struct {
	From  string
	To    string
	Gates []string
	When  []string
}

type ResetTo struct {
	Name             string
	Version          string
	Prompt           string
	RequiresApproval bool
	ClearHistory     bool
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
	ReasoningLevel string
	TextVerbosity  string
	RoleOverrides  map[string]RoleModelRouting
}

type RoleModelRouting struct {
	Model          string
	ReasoningLevel string
	TextVerbosity  string
}

type RunPhase string

const (
	RunPhasePending         RunPhase = "Pending"
	RunPhaseAdmitted        RunPhase = "Admitted"
	RunPhaseRunning         RunPhase = "Running"
	RunPhaseWaitingApproval RunPhase = "WaitingApproval"
	RunPhaseSucceeded       RunPhase = "Succeeded"
	RunPhaseFailed          RunPhase = "Failed"
)

type WorkflowMode string

const (
	WorkflowModeAuto WorkflowMode = "Auto"
	WorkflowModeChat WorkflowMode = "Chat"
)

// RunSnapshot contains the SDK-native fields needed for mode gate evaluation.
type RunSnapshot struct {
	WorkflowMode        WorkflowMode
	Phase               RunPhase
	ModeSnapshot        *TemplateSpec
	CompletionRequested bool
	Children            []ChildStatus
}

type ChildStatus struct {
	Name  string
	Phase RunPhase
}

// TemplateKey returns the registry key for a mode template.
func TemplateKey(name, version string) string {
	return name
}
