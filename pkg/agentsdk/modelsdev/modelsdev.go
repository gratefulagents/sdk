// Package modelsdev resolves per-model context-window limits from the
// models.dev open catalog (https://models.dev/api.json).
//
// Provider-hosted /models endpoints routinely under-report real limits (e.g.
// GitHub Copilot advertises a 200K prompt cap for claude-fable-5 while the
// deployment accepts 1M-context requests), so models.dev is treated as the
// authoritative source for compaction thresholds. The catalog is fetched
// lazily, cached in memory and on disk with a TTL, and served stale on fetch
// errors so offline runs keep working.
package modelsdev

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CatalogURL is the models.dev catalog endpoint.
	CatalogURL = "https://models.dev/api.json"

	defaultFetchTimeout = 20 * time.Second
	defaultCacheTTL     = 24 * time.Hour
	maxCatalogBytes     = 64 << 20
)

// ModelLimits are the context/output token limits for one model.
type ModelLimits struct {
	ContextTokens int `json:"context"`
	OutputTokens  int `json:"output"`
}

// Catalog is a parsed models.dev catalog: provider ID -> model ID -> limits.
type Catalog map[string]map[string]ModelLimits

// providerAliases maps runtime provider/prefix names to models.dev provider
// IDs. Both sides are matched lower-case. Note: "openai-oauth" (the ChatGPT
// codex backend at chatgpt.com/backend-api/codex) is deliberately absent —
// models.dev has no provider for that deployment and its windows differ from
// the OpenAI API (e.g. 272K vs 400K/1M), so lookups must miss and fall
// through to the codex backend's own /models metadata.
var providerAliases = map[string]string{
	"copilot":         "github-copilot",
	"github-copilot":  "github-copilot",
	"openai":          "openai",
	"anthropic":       "anthropic",
	"anthropic-oauth": "anthropic",
}

// Resolver lazily fetches and caches the models.dev catalog and answers
// per-(provider, model) limit lookups. Safe for concurrent use.
type Resolver struct {
	mu        sync.Mutex
	catalog   Catalog
	fetchedAt time.Time
	client    *http.Client
	cachePath string
	ttl       time.Duration
	url       string
}

// Option configures a Resolver.
type Option func(*Resolver)

// WithCachePath sets the on-disk cache location (default:
// os.UserCacheDir()/gratefulagents/modelsdev.json).
func WithCachePath(path string) Option { return func(r *Resolver) { r.cachePath = path } }

// WithTTL overrides the cache freshness window.
func WithTTL(ttl time.Duration) Option { return func(r *Resolver) { r.ttl = ttl } }

// WithHTTPClient overrides the HTTP client.
func WithHTTPClient(c *http.Client) Option { return func(r *Resolver) { r.client = c } }

// WithURL overrides the catalog URL (tests).
func WithURL(url string) Option { return func(r *Resolver) { r.url = url } }

// NewResolver builds a catalog resolver.
func NewResolver(opts ...Option) *Resolver {
	r := &Resolver{
		client: &http.Client{Timeout: defaultFetchTimeout},
		ttl:    defaultCacheTTL,
		url:    CatalogURL,
	}
	if dir, err := os.UserCacheDir(); err == nil {
		r.cachePath = filepath.Join(dir, "gratefulagents", "modelsdev.json")
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Lookup returns the limits for a model under a provider. The provider is an
// SDK provider or route prefix name ("copilot", "openai-oauth", ...); the
// model may carry a "prefix/" which is stripped for matching. Model IDs are
// matched case-insensitively, exact first and then with date/version suffix
// tolerance (catalog "claude-fable-5" matches request "claude-fable-5-20260601").
func (r *Resolver) Lookup(ctx context.Context, provider, model string) (ModelLimits, bool) {
	catalog := r.catalogSnapshot(ctx)
	if len(catalog) == 0 {
		return ModelLimits{}, false
	}
	providerID := providerAliases[strings.ToLower(strings.TrimSpace(provider))]
	if providerID == "" {
		providerID = strings.ToLower(strings.TrimSpace(provider))
	}
	models := catalog[providerID]
	if len(models) == 0 {
		return ModelLimits{}, false
	}
	id := normalizeModelID(model)
	if id == "" {
		return ModelLimits{}, false
	}
	if limits, ok := models[id]; ok && limits.ContextTokens > 0 {
		return limits, true
	}
	// Suffix tolerance: longest catalog ID that prefixes the requested ID
	// (or vice versa) wins, so dated snapshots resolve to their base model.
	bestLen := 0
	var best ModelLimits
	for candidate, limits := range models {
		if limits.ContextTokens <= 0 {
			continue
		}
		if strings.HasPrefix(id, candidate) || strings.HasPrefix(candidate, id) {
			if len(candidate) > bestLen {
				bestLen = len(candidate)
				best = limits
			}
		}
	}
	if bestLen > 0 {
		return best, true
	}
	return ModelLimits{}, false
}

// CompactionThresholds derives compaction trigger/target tokens from the
// model's context window: trigger at 90% (10% headroom for the next turn),
// target at 50%.
func (r *Resolver) CompactionThresholds(ctx context.Context, provider, model string) (triggerTokens, targetTokens int, ok bool) {
	limits, found := r.Lookup(ctx, provider, model)
	if !found || limits.ContextTokens <= 0 {
		return 0, 0, false
	}
	return (limits.ContextTokens * 9) / 10, limits.ContextTokens / 2, true
}

// CompactionResolverFunc adapts the resolver to the SDK's
// CompactionModelResolver shape. The model's route prefix (before "/")
// selects the provider; defaultProvider applies to unprefixed models.
func (r *Resolver) CompactionResolverFunc(defaultProvider string) func(ctx context.Context, model string) (int, int, bool) {
	return func(ctx context.Context, model string) (int, int, bool) {
		provider := defaultProvider
		if idx := strings.Index(strings.TrimSpace(model), "/"); idx > 0 {
			provider = strings.TrimSpace(model)[:idx]
		}
		return r.CompactionThresholds(ctx, provider, model)
	}
}

func normalizeModelID(model string) string {
	model = strings.TrimSpace(model)
	if idx := strings.Index(model, "/"); idx >= 0 {
		model = model[idx+1:]
	}
	return strings.ToLower(strings.TrimSpace(model))
}

// catalogSnapshot returns the current catalog, fetching or refreshing it if
// needed. Errors fall back to any previously loaded (memory or disk) copy.
func (r *Resolver) catalogSnapshot(ctx context.Context) Catalog {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.catalog != nil && time.Since(r.fetchedAt) < r.ttl {
		return r.catalog
	}
	if r.catalog == nil {
		if catalog, at, err := r.loadDiskCache(); err == nil {
			r.catalog = catalog
			r.fetchedAt = at
			if time.Since(at) < r.ttl {
				return r.catalog
			}
		}
	}
	catalog, err := r.fetch(ctx)
	if err != nil {
		// Serve stale on error; nil when nothing was ever loaded.
		return r.catalog
	}
	r.catalog = catalog
	r.fetchedAt = time.Now()
	r.saveDiskCache(catalog)
	return r.catalog
}

func (r *Resolver) fetch(ctx context.Context) (Catalog, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models.dev returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes))
	if err != nil {
		return nil, err
	}
	return ParseCatalog(body)
}

// ParseCatalog parses the models.dev api.json payload into a Catalog.
func ParseCatalog(body []byte) (Catalog, error) {
	var raw map[string]struct {
		Models map[string]struct {
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	catalog := make(Catalog, len(raw))
	for providerID, provider := range raw {
		if len(provider.Models) == 0 {
			continue
		}
		models := make(map[string]ModelLimits, len(provider.Models))
		for modelID, m := range provider.Models {
			id := strings.ToLower(strings.TrimSpace(modelID))
			if id == "" || m.Limit.Context <= 0 {
				continue
			}
			models[id] = ModelLimits{ContextTokens: m.Limit.Context, OutputTokens: m.Limit.Output}
		}
		if len(models) > 0 {
			catalog[strings.ToLower(strings.TrimSpace(providerID))] = models
		}
	}
	if len(catalog) == 0 {
		return nil, fmt.Errorf("models.dev catalog contained no models")
	}
	return catalog, nil
}

type diskCacheEnvelope struct {
	FetchedAt time.Time `json:"fetched_at"`
	Catalog   Catalog   `json:"catalog"`
}

func (r *Resolver) loadDiskCache() (Catalog, time.Time, error) {
	if r.cachePath == "" {
		return nil, time.Time{}, fmt.Errorf("no cache path")
	}
	data, err := os.ReadFile(r.cachePath)
	if err != nil {
		return nil, time.Time{}, err
	}
	var envelope diskCacheEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, time.Time{}, err
	}
	if len(envelope.Catalog) == 0 {
		return nil, time.Time{}, fmt.Errorf("empty cached catalog")
	}
	return envelope.Catalog, envelope.FetchedAt, nil
}

func (r *Resolver) saveDiskCache(catalog Catalog) {
	if r.cachePath == "" {
		return
	}
	data, err := json.Marshal(diskCacheEnvelope{FetchedAt: time.Now(), Catalog: catalog})
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(r.cachePath), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(r.cachePath, data, 0o644)
}
