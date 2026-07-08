package agent

import "context"

// Model is the provider-agnostic interface for LLM interaction.
// Implementations exist for Anthropic and OpenAI.
type Model interface {
	// GetResponse sends a request and returns a complete response.
	GetResponse(ctx context.Context, req ModelRequest) (*ModelResponse, error)

	// StreamResponse sends a request and returns a streaming response.
	StreamResponse(ctx context.Context, req ModelRequest) (*ModelStream, error)

	// GetRetryAdvice determines whether a failed call should be retried.
	GetRetryAdvice(err error) *ModelRetryAdvice

	// CalculateCost returns the USD cost for the given usage.
	CalculateCost(usage Usage) float64

	// Provider returns the provider name ("anthropic" or "openai").
	Provider() string
}

// ContextCompactor is implemented by models that support provider-native
// context compaction, such as OpenAI Responses compaction.
type ContextCompactor interface {
	SupportsContextCompaction() bool
	CompactContext(ctx context.Context, req ModelRequest) (*CompactionResult, error)
}

// UsageCacheSemantics is implemented by models to declare how their provider
// reports prompt-side usage. OpenAI-style usage reports input_tokens
// INCLUSIVE of cached tokens (cached_tokens is a subset), while
// Anthropic-style usage reports input_tokens EXCLUSIVE of cache reads and
// creations (they are additive fields). Models that do not implement this
// interface are treated as Anthropic-style (additive).
type UsageCacheSemantics interface {
	// UsageInputIncludesCacheTokens returns true when Usage.InputTokens
	// already contains the cache read/creation tokens for the request.
	UsageInputIncludesCacheTokens() bool
}

// usageContextTokens returns the total prompt-side tokens one request
// actually consumed, normalizing provider usage semantics. Summing cache
// fields on top of OpenAI-style input_tokens double-counts the cached prompt
// (nearly the whole context on warm requests) — that inflated the estimator
// calibration and made compaction fire at ~half the real window.
func usageContextTokens(model Model, usage Usage) int64 {
	if semantics, ok := model.(UsageCacheSemantics); ok && semantics.UsageInputIncludesCacheTokens() {
		return usage.InputTokens
	}
	return usage.InputTokens + usage.CacheReadTokens + usage.CacheCreateTokens
}

// ModelRequest contains everything needed for a model call.
type ModelRequest struct {
	Model               string
	Instructions        string
	Input               []RunItem
	Tools               []Tool
	Settings            ModelSettings
	OutputSchema        *OutputSchema
	CompactionThreshold int
}

// ModelResponse is the complete response from a model call.
type ModelResponse struct {
	Items     []RunItem
	Usage     Usage
	Raw       any     // provider-specific raw response for debugging
	CostUSD   float64 // estimated cost in USD (set by runner after model returns)
	CostKnown bool    // whether cost estimate is considered reliable
}

// CompactionResult is the provider-native compacted context window.
type CompactionResult struct {
	Items   []RunItem
	Usage   Usage
	Raw     any
	Summary string
}

// ToRunItems returns the items from this response.
func (r *ModelResponse) ToRunItems() []RunItem {
	return r.Items
}

// ModelStream provides streaming access to a model response.
type ModelStream struct {
	Events <-chan ModelStreamEvent
	done   chan *ModelResponse
}

func NewModelStream(events <-chan ModelStreamEvent, done chan *ModelResponse) *ModelStream {
	return &ModelStream{Events: events, done: done}
}

// ModelStreamEventType identifies the kind of model stream event.
type ModelStreamEventType int

const (
	ModelStreamDelta    ModelStreamEventType = iota // partial text/tool content
	ModelStreamItemDone                             // complete item ready
	ModelStreamComplete                             // stream finished
	ModelStreamError                                // terminal stream error
	ModelStreamReasoningDelta                       // partial reasoning/thinking content
)

// ModelStreamEvent is a single event from a streaming model call.
type ModelStreamEvent struct {
	Type     ModelStreamEventType
	Delta    string         // for Delta events
	Item     *RunItem       // for ItemDone events
	Response *ModelResponse // for Complete events
	Error    error          // for Error events
}

// Final blocks until the stream completes and returns the full response.
func (s *ModelStream) Final() *ModelResponse {
	return <-s.done
}
