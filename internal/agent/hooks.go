package agent

// RunHooks defines run-level lifecycle callbacks.
// These are called once per run, not per agent within a run.
type RunHooks interface {
	OnAgentStart(ctx *RunContext, agent *Agent)
	OnAgentEnd(ctx *RunContext, agent *Agent, output any)
	OnHandoff(ctx *RunContext, from *Agent, to *Agent)
	OnToolStart(ctx *RunContext, agent *Agent, tool Tool, call ToolCallData)
	OnToolEnd(ctx *RunContext, agent *Agent, tool Tool, call ToolCallData, result ToolResult)
	OnLLMStart(ctx *RunContext, agent *Agent)
	OnLLMEnd(ctx *RunContext, agent *Agent, response *ModelResponse)
}

// AgentHooks defines per-agent lifecycle callbacks.
type AgentHooks interface {
	OnStart(ctx *RunContext, agent *Agent)
	OnEnd(ctx *RunContext, agent *Agent, output any)
	OnHandoff(ctx *RunContext, agent *Agent, target *Agent)
	OnToolStart(ctx *RunContext, agent *Agent, tool Tool, call ToolCallData)
	OnToolEnd(ctx *RunContext, agent *Agent, tool Tool, call ToolCallData, result ToolResult)
}

// NoOpRunHooks is a RunHooks implementation that does nothing.
type NoOpRunHooks struct{}

func (NoOpRunHooks) OnAgentStart(_ *RunContext, _ *Agent)                                    {}
func (NoOpRunHooks) OnAgentEnd(_ *RunContext, _ *Agent, _ any)                               {}
func (NoOpRunHooks) OnHandoff(_ *RunContext, _ *Agent, _ *Agent)                             {}
func (NoOpRunHooks) OnToolStart(_ *RunContext, _ *Agent, _ Tool, _ ToolCallData)             {}
func (NoOpRunHooks) OnToolEnd(_ *RunContext, _ *Agent, _ Tool, _ ToolCallData, _ ToolResult) {}
func (NoOpRunHooks) OnLLMStart(_ *RunContext, _ *Agent)                                      {}
func (NoOpRunHooks) OnLLMEnd(_ *RunContext, _ *Agent, _ *ModelResponse)                      {}

// NoOpAgentHooks is an AgentHooks implementation that does nothing.
type NoOpAgentHooks struct{}

func (NoOpAgentHooks) OnStart(_ *RunContext, _ *Agent)                                         {}
func (NoOpAgentHooks) OnEnd(_ *RunContext, _ *Agent, _ any)                                    {}
func (NoOpAgentHooks) OnHandoff(_ *RunContext, _ *Agent, _ *Agent)                             {}
func (NoOpAgentHooks) OnToolStart(_ *RunContext, _ *Agent, _ Tool, _ ToolCallData)             {}
func (NoOpAgentHooks) OnToolEnd(_ *RunContext, _ *Agent, _ Tool, _ ToolCallData, _ ToolResult) {}
