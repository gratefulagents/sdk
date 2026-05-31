package agent

// ModelProvider resolves model name strings to Model implementations.
// This is the extension point for adding new LLM providers.
type ModelProvider interface {
	// GetModel returns a Model for the given name.
	// The name may be a bare model identifier (e.g. "claude-sonnet-4-6")
	// or include a provider prefix (e.g. "anthropic/claude-sonnet-4-6").
	GetModel(name string) (Model, error)

	// Close releases any resources held by this provider.
	Close() error
}
