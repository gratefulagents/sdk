package agent

import (
	"encoding/json"
	"fmt"
)

// OutputSchema defines a structured output format for an Agent.
type OutputSchema struct {
	Name    string          `json:"name"`
	Schema  json.RawMessage `json:"schema"`
	Strict  bool            `json:"strict"`
	ParseFn func(raw string) (any, error)
}

// NewOutputSchema creates a new output schema with strict validation enabled.
func NewOutputSchema(name string, schema json.RawMessage) *OutputSchema {
	return &OutputSchema{
		Name:   name,
		Schema: schema,
		Strict: true,
	}
}

// Validate checks that raw JSON conforms to the schema by attempting to parse it.
// Returns the parsed value on success, or an error describing the validation failure.
func (s *OutputSchema) Validate(raw string) (any, error) {
	if s.ParseFn != nil {
		return s.ParseFn(raw)
	}
	// Fallback: verify it's valid JSON.
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("output schema %q: invalid JSON: %w", s.Name, err)
	}
	return v, nil
}
