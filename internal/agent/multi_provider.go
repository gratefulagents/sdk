package agent

import (
	"fmt"
	"strings"
)

// MultiProvider dispatches model requests to registered providers by prefix.
//
// Model names may be prefixed with a provider name and a slash:
//
//	"anthropic/claude-sonnet-4-6" → AnthropicProvider with model "claude-sonnet-4-6"
//	"openai/gpt-4.1"             → OpenAIProvider with model "gpt-4.1"
//	"gpt-4.1"                    → default provider (openai) with model "gpt-4.1"
type MultiProvider struct {
	providers     map[string]ModelProvider
	defaultPrefix string
}

// NewMultiProvider creates a MultiProvider with the given default prefix.
// The default prefix is used when a model name contains no "/" separator.
func NewMultiProvider(defaultPrefix string) *MultiProvider {
	return &MultiProvider{
		providers:     make(map[string]ModelProvider),
		defaultPrefix: defaultPrefix,
	}
}

// Register adds a provider under the given prefix.
func (mp *MultiProvider) Register(prefix string, p ModelProvider) {
	mp.providers[prefix] = p
}

// GetModel parses the model name for a provider prefix and delegates to the
// appropriate registered provider.
func (mp *MultiProvider) GetModel(name string) (Model, error) {
	prefix, modelName := ParseModelPrefix(name)
	if prefix == "" {
		prefix = mp.defaultPrefix
		modelName = name
	}
	p, ok := mp.providers[prefix]
	if !ok {
		known := make([]string, 0, len(mp.providers))
		for k := range mp.providers {
			known = append(known, k)
		}
		return nil, &AgentError{
			Message: fmt.Sprintf("unknown model provider prefix %q (known: %s)", prefix, strings.Join(known, ", ")),
		}
	}
	return p.GetModel(modelName)
}

// Close releases resources for all registered providers.
func (mp *MultiProvider) Close() error {
	var firstErr error
	for _, p := range mp.providers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ModelNameNormalizer is an optional ModelProvider extension. Providers that
// route by model-name prefix implement it to report the model name that
// should be sent in API requests after routing.
type ModelNameNormalizer interface {
	NormalizeModelName(name string) string
}

// NormalizeModelName strips the provider prefix only when it routes to a
// registered provider, preserving model IDs that contain "/" as part of the
// ID (e.g. "openrouter/anthropic/claude-..." → "anthropic/claude-...", while
// an unregistered "anthropic/claude-..." prefix stays intact).
func (mp *MultiProvider) NormalizeModelName(name string) string {
	prefix, bare := ParseModelPrefix(name)
	if prefix == "" || bare == "" {
		return name
	}
	if _, ok := mp.providers[prefix]; ok {
		return bare
	}
	return name
}

// ParseModelPrefix splits a "prefix/model" string into its components.
// If there is no "/" the prefix is empty and model is the full string.
func ParseModelPrefix(name string) (prefix, model string) {
	name = strings.TrimSpace(name)
	if i := strings.Index(name, "/"); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}
