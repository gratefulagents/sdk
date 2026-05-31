package agent

// Usage tracks token consumption across model calls.
type Usage struct {
	Requests          int   `json:"requests"`
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CacheReadTokens   int64 `json:"cache_read_tokens,omitempty"`
	CacheCreateTokens int64 `json:"cache_create_tokens,omitempty"`
}

// TotalTokens returns input + output tokens.
func (u *Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// Add merges another Usage into this one.
func (u *Usage) Add(other Usage) {
	u.Requests += other.Requests
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.CacheReadTokens += other.CacheReadTokens
	u.CacheCreateTokens += other.CacheCreateTokens
}
