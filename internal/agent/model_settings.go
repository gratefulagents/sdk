package agent

// ModelSettings configures model behavior.
type ModelSettings struct {
	Temperature       *float64 `json:"temperature,omitempty"`
	MaxTokens         int      `json:"max_tokens,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	ToolChoice        string   `json:"tool_choice,omitempty"` // "auto", "none", "required", or specific tool name
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	ThinkingBudget    int      `json:"thinking_budget,omitempty"`
	ReasoningEffort   string   `json:"reasoning_effort,omitempty"` // "minimal", "low", "medium", "high", "xhigh"
	TextVerbosity     string   `json:"text_verbosity,omitempty"`   // "low", "medium", "high"
	StopSequences     []string `json:"stop_sequences,omitempty"`
}

// Merge returns a new ModelSettings with overrides from other applied.
// Non-zero/non-nil values in other take precedence.
func (s ModelSettings) Merge(other ModelSettings) ModelSettings {
	merged := s
	if other.Temperature != nil {
		merged.Temperature = other.Temperature
	}
	if other.MaxTokens > 0 {
		merged.MaxTokens = other.MaxTokens
	}
	if other.TopP != nil {
		merged.TopP = other.TopP
	}
	if other.ToolChoice != "" {
		merged.ToolChoice = other.ToolChoice
	}
	if other.ParallelToolCalls != nil {
		merged.ParallelToolCalls = other.ParallelToolCalls
	}
	if other.ThinkingBudget > 0 {
		merged.ThinkingBudget = other.ThinkingBudget
	}
	if other.ReasoningEffort != "" {
		merged.ReasoningEffort = other.ReasoningEffort
	}
	if other.TextVerbosity != "" {
		merged.TextVerbosity = other.TextVerbosity
	}
	if len(other.StopSequences) > 0 {
		merged.StopSequences = other.StopSequences
	}
	return merged
}

// ModelRetryAdvice is returned by Model.GetRetryAdvice to indicate whether a failed call should be retried.
type ModelRetryAdvice struct {
	ShouldRetry  bool
	RetryAfterMS int64
	Reason       string
}
