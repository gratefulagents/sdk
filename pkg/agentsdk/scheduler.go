package agentsdk

import "github.com/gratefulagents/sdk/internal/agent"

// SubAgentSchedulerConfig configures the SDK async sub-agent scheduler.
type SubAgentSchedulerConfig struct {
	MaxConcurrent           int
	Runner                  *Runner
	Agents                  map[string]*Agent
	Tracker                 *RunProgress
	EventStream             *EventStream
	EventWriter             *EventStream
	WorkDir                 string
	ToolAccessLevel         ToolAccessLevel
	ToolPolicy              *ToolPolicy
	CompactionConfig        CompactionConfig
	CompactionModelResolver CompactionModelResolver
	MaxTurns                int
}

// NewSubAgentScheduler creates a scheduler for async sub-agent tasks.
func NewSubAgentScheduler(cfg SubAgentSchedulerConfig) *SubAgentScheduler {
	eventStream := cfg.EventStream
	if eventStream == nil {
		eventStream = cfg.EventWriter
	}
	return agent.NewSubAgentRegistry(agent.SubAgentRegistryConfig{
		MaxConcurrent:           cfg.MaxConcurrent,
		Runner:                  cfg.Runner,
		Agents:                  cfg.Agents,
		Tracker:                 cfg.Tracker,
		EventStream:             eventStream,
		WorkDir:                 cfg.WorkDir,
		ToolAccessLevel:         cfg.ToolAccessLevel,
		ToolPolicy:              cfg.ToolPolicy,
		CompactionConfig:        cfg.CompactionConfig,
		CompactionModelResolver: cfg.CompactionModelResolver,
		MaxTurns:                cfg.MaxTurns,
	})
}

// ConfigureSubAgentScheduler refreshes host/runtime wiring for an existing
// scheduler while preserving already tracked async tasks.
func ConfigureSubAgentScheduler(s *SubAgentScheduler, cfg SubAgentSchedulerConfig) {
	if s == nil {
		return
	}
	eventStream := cfg.EventStream
	if eventStream == nil {
		eventStream = cfg.EventWriter
	}
	s.Configure(agent.SubAgentRegistryConfig{
		MaxConcurrent:           cfg.MaxConcurrent,
		Runner:                  cfg.Runner,
		Agents:                  cfg.Agents,
		Tracker:                 cfg.Tracker,
		EventStream:             eventStream,
		WorkDir:                 cfg.WorkDir,
		ToolAccessLevel:         cfg.ToolAccessLevel,
		ToolPolicy:              cfg.ToolPolicy,
		CompactionConfig:        cfg.CompactionConfig,
		CompactionModelResolver: cfg.CompactionModelResolver,
		MaxTurns:                cfg.MaxTurns,
	})
}
