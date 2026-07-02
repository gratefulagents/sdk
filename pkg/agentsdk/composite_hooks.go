package agentsdk

// CompositeHooks fans out RunHooks calls to multiple implementations.
// This allows hosts to observe lifecycle events with several independent sinks.
type CompositeHooks struct {
	hooks []RunHooks
}

// NewCompositeHooks creates a CompositeHooks that delegates to all provided hooks.
func NewCompositeHooks(hooks ...RunHooks) *CompositeHooks {
	filtered := make([]RunHooks, 0, len(hooks))
	for _, h := range hooks {
		if h != nil {
			filtered = append(filtered, h)
		}
	}
	return &CompositeHooks{hooks: filtered}
}

// Unwrap exposes the wrapped hooks so runner internals that look for a
// specific hook implementation (e.g. PlatformHooks for llm_attempt event
// emission) can find it inside a composite.
func (c *CompositeHooks) Unwrap() []RunHooks {
	return c.hooks
}

func (c *CompositeHooks) OnAgentStart(ctx *RunContext, a *Agent) {
	for _, h := range c.hooks {
		h.OnAgentStart(ctx, a)
	}
}

func (c *CompositeHooks) OnAgentEnd(ctx *RunContext, a *Agent, output any) {
	for _, h := range c.hooks {
		h.OnAgentEnd(ctx, a, output)
	}
}

func (c *CompositeHooks) OnHandoff(ctx *RunContext, from *Agent, to *Agent) {
	for _, h := range c.hooks {
		h.OnHandoff(ctx, from, to)
	}
}

func (c *CompositeHooks) OnToolStart(ctx *RunContext, a *Agent, tool Tool, call ToolCallData) {
	for _, h := range c.hooks {
		h.OnToolStart(ctx, a, tool, call)
	}
}

func (c *CompositeHooks) OnToolEnd(ctx *RunContext, a *Agent, tool Tool, call ToolCallData, result ToolResult) {
	for _, h := range c.hooks {
		h.OnToolEnd(ctx, a, tool, call, result)
	}
}

func (c *CompositeHooks) OnLLMStart(ctx *RunContext, a *Agent) {
	for _, h := range c.hooks {
		h.OnLLMStart(ctx, a)
	}
}

func (c *CompositeHooks) OnLLMEnd(ctx *RunContext, a *Agent, response *ModelResponse) {
	for _, h := range c.hooks {
		h.OnLLMEnd(ctx, a, response)
	}
}
