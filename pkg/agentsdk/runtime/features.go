package runtime

// Features is the explicit runtime feature surface. All zero values are off.
//
// Config.Features is intentionally a pointer: nil preserves the legacy
// compatibility behavior driven by Config.Enable*/Disable* fields, while a
// non-nil Features value opts into strict explicit selection.
type Features struct {
	Tools        ToolFeatures
	MCP          MCPFeatures
	Handoffs     HandoffFeatures
	SubAgents    SubAgentFeatures
	Guardrails   GuardrailFeatures
	Modes        ModeFeatures
	ProjectState ProjectStateFeatures
	Runtime      RuntimeFeatures
}

type ToolFeatures struct {
	ListFiles        bool
	ReadFile         bool
	Glob             bool
	Grep             bool
	LSP              bool
	Bash             bool
	Write            bool
	Edit             bool
	WebFetch         bool
	AsyncShell       bool
	AttachRepository bool
	ExtraTools       bool
	VisionAnalyzer   bool
	Signals          SignalFeatures
}

type SignalFeatures struct {
	AskUserQuestion bool
	PresentPlan     bool
	Finish          bool
}

type MCPFeatures struct {
	Enabled         bool
	AllowAllServers bool
	AllowedServers  []string
	AllowAllTools   bool
	AllowedTools    []string
	ResourceTools   bool
}

type HandoffFeatures struct {
	Enabled         bool
	GenericFallback bool
}

type SubAgentFeatures struct {
	GenericFallback bool
	Async           AsyncSubAgentFeatures
}

// AsyncSubAgentFeatures gates the managed sub-agent tool surface:
// subagent (spawn sync/background, single or DAG), subagent_status
// (summary/activity/graph introspection), subagent_control (steer/cancel).
type AsyncSubAgentFeatures struct {
	Task    bool
	Status  bool
	Control bool
}

type GuardrailFeatures struct {
	Builtin bool
}

type ModeFeatures struct {
	Instructions bool
	ModelRouting bool
}

type ProjectStateFeatures struct {
	PrimeContext bool
	TaskTools    bool
	MemoryTools  bool
	PrimeTool    bool
}

type RuntimeFeatures struct {
	Compaction            bool
	Approval              bool
	Retry                 bool
	ForceFinalSummary     bool
	EventStream           bool
	Tracing               bool
	ImmediateInputPolling bool
	HandoffHistory        bool
	ParallelToolCalls     bool
	UntrustedToolOutputs  bool
}

type resolvedFeatures struct {
	strict bool
	value  Features
}

func resolveFeatures(cfg Config) resolvedFeatures {
	if cfg.Features != nil {
		return resolvedFeatures{strict: true, value: *cfg.Features}
	}
	return resolvedFeatures{value: legacyFeatures(cfg)}
}

func legacyFeatures(cfg Config) Features {
	defaultTools := cfg.EnableTools && !cfg.DisableDefaultTools
	signalTools := (cfg.EnableTools || cfg.EnableSubAgents) && !cfg.DisableSignalTools
	return Features{
		Tools: ToolFeatures{
			ListFiles:      defaultTools,
			ReadFile:       defaultTools,
			Glob:           defaultTools,
			Grep:           defaultTools,
			LSP:            defaultTools,
			Bash:           defaultTools,
			Write:          defaultTools,
			Edit:           defaultTools,
			WebFetch:       defaultTools && !cfg.DisableWebTools,
			AsyncShell:     defaultTools && cfg.EnableAsyncShell,
			ExtraTools:     cfg.EnableTools || cfg.EnableSubAgents,
			VisionAnalyzer: cfg.EnableTools || cfg.EnableSubAgents,
			Signals: SignalFeatures{
				AskUserQuestion: signalTools,
				PresentPlan:     signalTools,
				Finish:          signalTools,
			},
		},
		MCP: MCPFeatures{
			Enabled:         cfg.EnableMCP,
			AllowAllServers: cfg.EnableMCP,
			AllowAllTools:   cfg.EnableMCP,
			ResourceTools:   cfg.EnableMCP,
		},
		Handoffs: HandoffFeatures{
			Enabled:         cfg.EnableHandoffs,
			GenericFallback: cfg.EnableHandoffs,
		},
		SubAgents: SubAgentFeatures{
			GenericFallback: cfg.EnableSubAgents,
			Async: AsyncSubAgentFeatures{
				Task:    cfg.EnableSubAgents,
				Status:  cfg.EnableSubAgents,
				Control: cfg.EnableSubAgents,
			},
		},
		Guardrails: GuardrailFeatures{
			Builtin: cfg.EnableGuardrails,
		},
		Modes: ModeFeatures{
			Instructions: true,
			ModelRouting: true,
		},
		ProjectState: ProjectStateFeatures{
			PrimeContext: cfg.EnableProjectState,
			TaskTools:    cfg.EnableProjectState,
			MemoryTools:  cfg.EnableProjectState,
			PrimeTool:    cfg.EnableProjectState,
		},
		Runtime: RuntimeFeatures{
			Compaction:            cfg.EnableCompaction,
			Approval:              cfg.EnableApproval,
			Retry:                 cfg.EnableRetry,
			ForceFinalSummary:     cfg.ForceFinalSummary,
			EventStream:           cfg.EventWriter != nil,
			Tracing:               cfg.TracingProcessor != nil,
			ImmediateInputPolling: cfg.ImmediateInputPoller != nil,
			HandoffHistory:        cfg.EnableCompaction,
			ParallelToolCalls:     true,
			UntrustedToolOutputs:  true,
		},
	}
}

func (f ToolFeatures) hasRegistryTools() bool {
	return f.ListFiles || f.ReadFile || f.Glob || f.Grep || f.LSP || f.Bash || f.Write || f.Edit || f.WebFetch || f.AsyncShell || f.AttachRepository
}

func (f ToolFeatures) hasSignals() bool {
	return f.Signals.AskUserQuestion || f.Signals.PresentPlan || f.Signals.Finish
}

func (f MCPFeatures) hasServerSelection() bool {
	return f.AllowAllServers || len(f.AllowedServers) > 0
}

func (f MCPFeatures) hasToolSelection() bool {
	return f.AllowAllTools || len(f.AllowedTools) > 0 || f.ResourceTools
}

func (f ProjectStateFeatures) needsStore() bool {
	return f.PrimeContext || f.TaskTools || f.MemoryTools || f.PrimeTool
}

func (f SubAgentFeatures) enabled() bool {
	return f.asyncEnabled()
}

func (f SubAgentFeatures) asyncEnabled() bool {
	return f.Async.any()
}

func (f AsyncSubAgentFeatures) any() bool {
	return f.Task || f.Status || f.Control
}

func shouldBuildToolBundle(features resolvedFeatures) bool {
	f := features.value
	return f.Tools.hasRegistryTools() ||
		f.Tools.hasSignals() ||
		f.Tools.ExtraTools ||
		(f.MCP.Enabled && f.MCP.hasServerSelection() && f.MCP.hasToolSelection()) ||
		f.ProjectState.TaskTools ||
		f.ProjectState.MemoryTools ||
		f.ProjectState.PrimeTool ||
		f.SubAgents.enabled()
}
