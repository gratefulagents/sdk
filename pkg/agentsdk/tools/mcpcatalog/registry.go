package mcpcatalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed default_catalog.json
var defaultCatalogJSON []byte

// CatalogEntry represents one installable MCP server in the catalog.
type CatalogEntry struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Category        string          `json:"category"`
	Version         string          `json:"version"`
	Source          CatalogSource     `json:"source"`
	MCPConfig       MCPServerConfig `json:"mcpConfig"`
	Tags            []string        `json:"tags,omitempty"`
	Verified        bool            `json:"verified"`
	RequiresEnvVars []string        `json:"requiresEnvVars,omitempty"`
}

// CatalogSource identifies where a catalog entry's package comes from.
type CatalogSource struct {
	Repository string `json:"repository,omitempty"`
	Ref        string `json:"ref,omitempty"`
	URL        string `json:"url,omitempty"`
}

// MCPServerConfig defines how to run an MCP server.
type MCPServerConfig struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

// Registry manages the MCP server catalog for discovery and installation.
type Registry struct {
	entries []CatalogEntry
}

// NewRegistry creates a registry loaded with the default embedded catalog.
func NewRegistry() (*Registry, error) {
	entries, err := LoadDefaultCatalog()
	if err != nil {
		return nil, fmt.Errorf("loading default catalog: %w", err)
	}
	return &Registry{entries: entries}, nil
}

// NewRegistryFromEntries creates a registry from provided entries.
func NewRegistryFromEntries(entries []CatalogEntry) *Registry {
	return &Registry{entries: entries}
}

// LoadDefaultCatalog parses the embedded default catalog.
func LoadDefaultCatalog() ([]CatalogEntry, error) {
	var catalog struct {
		Entries []CatalogEntry `json:"entries"`
	}
	if err := json.Unmarshal(defaultCatalogJSON, &catalog); err != nil {
		return nil, fmt.Errorf("parsing catalog: %w", err)
	}
	return catalog.Entries, nil
}

// FilterOption configures catalog entry filtering.
type FilterOption func(*filterConfig)

type filterConfig struct {
	category string
	tags     []string
	verified *bool
}

// WithCategory filters entries by category.
func WithCategory(cat string) FilterOption {
	return func(c *filterConfig) { c.category = cat }
}

// WithTags filters entries that have any of the given tags.
func WithTags(tags ...string) FilterOption {
	return func(c *filterConfig) { c.tags = tags }
}

// WithVerifiedOnly filters to only verified entries.
func WithVerifiedOnly() FilterOption {
	return func(c *filterConfig) { v := true; c.verified = &v }
}

// List returns all entries, optionally filtered.
func (r *Registry) List(opts ...FilterOption) []CatalogEntry {
	cfg := &filterConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	var result []CatalogEntry
	for _, s := range r.entries {
		if cfg.category != "" && !strings.EqualFold(s.Category, cfg.category) {
			continue
		}
		if cfg.verified != nil && *cfg.verified && !s.Verified {
			continue
		}
		if len(cfg.tags) > 0 && !hasAnyTag(s.Tags, cfg.tags) {
			continue
		}
		result = append(result, s)
	}
	return result
}

// Get returns an entry by name, or nil if not found.
func (r *Registry) Get(name string) (*CatalogEntry, bool) {
	for _, s := range r.entries {
		if s.Name == name {
			return &s, true
		}
	}
	return nil, false
}

// Search finds entries matching a query string against name, description, and tags.
func (r *Registry) Search(query string) []CatalogEntry {
	q := strings.ToLower(query)
	var results []CatalogEntry
	for _, s := range r.entries {
		if strings.Contains(strings.ToLower(s.Name), q) ||
			strings.Contains(strings.ToLower(s.Description), q) ||
			containsTag(s.Tags, q) {
			results = append(results, s)
		}
	}
	return results
}

// Categories returns all unique categories in the catalog.
func (r *Registry) Categories() []string {
	seen := make(map[string]bool)
	var cats []string
	for _, s := range r.entries {
		if s.Category != "" && !seen[s.Category] {
			seen[s.Category] = true
			cats = append(cats, s.Category)
		}
	}
	return cats
}

func hasAnyTag(entryTags, filterTags []string) bool {
	for _, ft := range filterTags {
		for _, st := range entryTags {
			if strings.EqualFold(st, ft) {
				return true
			}
		}
	}
	return false
}

func containsTag(tags []string, query string) bool {
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), query) {
			return true
		}
	}
	return false
}
