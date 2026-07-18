package agent

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// MultiProvider dispatches model requests to registered providers by prefix.
//
// Model names may be prefixed with a provider name and a slash:
//
//	"anthropic/claude-sonnet-4-6" → AnthropicProvider with model "claude-sonnet-4-6"
//	"openai/gpt-4.1"             → OpenAIProvider with model "gpt-4.1"
//	"gpt-4.1"                    → default provider (openai) with model "gpt-4.1"
type MultiProvider struct {
	mu            sync.RWMutex
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
	mp.mu.Lock()
	defer mp.mu.Unlock()
	mp.providers[prefix] = p
}

// GetModel parses the model name for a provider prefix and delegates to the
// appropriate registered provider. Names whose first "/" segment is not a
// registered provider prefix are rejected; model IDs that themselves contain
// "/" must be double-prefixed (e.g. "openrouter/anthropic/claude-...").
func (mp *MultiProvider) GetModel(name string) (Model, error) {
	prefix, modelName := ParseModelPrefix(name)
	if prefix == "" {
		prefix = mp.defaultPrefix
	}
	mp.mu.RLock()
	p, ok := mp.providers[prefix]
	if !ok {
		known := make([]string, 0, len(mp.providers))
		for k := range mp.providers {
			known = append(known, k)
		}
		mp.mu.RUnlock()
		sort.Strings(known)
		return nil, &AgentError{
			Message: fmt.Sprintf("unknown model provider prefix %q (known: %s)", prefix, strings.Join(known, ", ")),
		}
	}
	mp.mu.RUnlock()
	return p.GetModel(modelName)
}

// Close releases resources for all registered providers.
func (mp *MultiProvider) Close() error {
	mp.mu.RLock()
	providers := make([]ModelProvider, 0, len(mp.providers))
	for _, p := range mp.providers {
		providers = append(providers, p)
	}
	mp.mu.RUnlock()
	var firstErr error
	for _, p := range providers {
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
// ID (e.g. "openrouter/anthropic/claude-..." → "anthropic/claude-...").
// Names with an unregistered prefix are returned unchanged, but note GetModel
// rejects them: slashed model IDs are only routable when double-prefixed with
// a registered provider.
func (mp *MultiProvider) NormalizeModelName(name string) string {
	prefix, bare := ParseModelPrefix(name)
	if prefix == "" || bare == "" {
		return name
	}
	mp.mu.RLock()
	_, ok := mp.providers[prefix]
	mp.mu.RUnlock()
	if ok {
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
