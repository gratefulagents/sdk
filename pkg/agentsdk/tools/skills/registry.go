package skills

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
)

//go:embed default_catalog.json
var defaultCatalogJSON []byte

// SkillEntry represents a single skill in the catalog.
type SkillEntry struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Category        string          `json:"category"`
	Version         string          `json:"version"`
	Source          SkillSource     `json:"source"`
	MCPConfig       MCPServerConfig `json:"mcpConfig"`
	Tags            []string        `json:"tags,omitempty"`
	Verified        bool            `json:"verified"`
	RequiresEnvVars []string        `json:"requiresEnvVars,omitempty"`
}

// SkillSource identifies where a skill package comes from.
type SkillSource struct {
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

// Registry manages the skill catalog for discovery and installation.
type Registry struct {
	skills []SkillEntry
}

// NewRegistry creates a registry loaded with the default embedded catalog.
func NewRegistry() (*Registry, error) {
	entries, err := LoadDefaultCatalog()
	if err != nil {
		return nil, fmt.Errorf("loading default catalog: %w", err)
	}
	return &Registry{skills: entries}, nil
}

// NewRegistryFromEntries creates a registry from provided entries.
func NewRegistryFromEntries(entries []SkillEntry) *Registry {
	return &Registry{skills: entries}
}

// LoadDefaultCatalog parses the embedded default skill catalog.
func LoadDefaultCatalog() ([]SkillEntry, error) {
	var catalog struct {
		Skills []SkillEntry `json:"skills"`
	}
	if err := json.Unmarshal(defaultCatalogJSON, &catalog); err != nil {
		return nil, fmt.Errorf("parsing catalog: %w", err)
	}
	return catalog.Skills, nil
}

// FilterOption configures skill filtering.
type FilterOption func(*filterConfig)

type filterConfig struct {
	category string
	tags     []string
	verified *bool
}

// WithCategory filters skills by category.
func WithCategory(cat string) FilterOption {
	return func(c *filterConfig) { c.category = cat }
}

// WithTags filters skills that have any of the given tags.
func WithTags(tags ...string) FilterOption {
	return func(c *filterConfig) { c.tags = tags }
}

// WithVerifiedOnly filters to only verified skills.
func WithVerifiedOnly() FilterOption {
	return func(c *filterConfig) { v := true; c.verified = &v }
}

// List returns all skills, optionally filtered.
func (r *Registry) List(opts ...FilterOption) []SkillEntry {
	cfg := &filterConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	var result []SkillEntry
	for _, s := range r.skills {
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

// Get returns a skill by name, or nil if not found.
func (r *Registry) Get(name string) (*SkillEntry, bool) {
	for _, s := range r.skills {
		if s.Name == name {
			return &s, true
		}
	}
	return nil, false
}

// Search finds skills matching a query string against name, description, and tags.
func (r *Registry) Search(query string) []SkillEntry {
	q := strings.ToLower(query)
	var results []SkillEntry
	for _, s := range r.skills {
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
	for _, s := range r.skills {
		if s.Category != "" && !seen[s.Category] {
			seen[s.Category] = true
			cats = append(cats, s.Category)
		}
	}
	return cats
}

func hasAnyTag(skillTags, filterTags []string) bool {
	for _, ft := range filterTags {
		for _, st := range skillTags {
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
