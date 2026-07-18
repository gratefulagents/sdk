package openai

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// CompactionMetadataResolver fetches provider model metadata once and uses it
// to derive SDK compaction defaults for OpenAI-compatible models. A failed
// fetch (e.g. a cancelled first caller or a transient network error) is
// retried after a cooldown instead of being cached for the process lifetime.
type CompactionMetadataResolver struct {
	baseURL string
	session *AuthSession

	fetchMu     sync.Mutex
	fetched     bool
	byID        map[string]ModelMetadata
	lastErr     error
	lastAttempt time.Time

	mu     sync.Mutex
	logged map[string]struct{}
}

// compactionMetadataRetryCooldown throttles re-fetch attempts after a failed
// model-metadata fetch.
const compactionMetadataRetryCooldown = 30 * time.Second

func NewCompactionMetadataResolver(baseURL string, session ...*AuthSession) *CompactionMetadataResolver {
	var authSession *AuthSession
	if len(session) > 0 {
		authSession = session[0]
	}
	return &CompactionMetadataResolver{
		baseURL: strings.TrimSpace(baseURL),
		session: authSession,
		logged:  make(map[string]struct{}),
	}
}

func (r *CompactionMetadataResolver) Lookup(ctx context.Context, model string) (ModelMetadata, bool) {
	if r == nil {
		return ModelMetadata{}, false
	}
	byID, err := r.metadataByID(ctx)
	if err != nil {
		r.LogOnce("fetch-error", "WARN: failed to fetch OpenAI model metadata for compaction: %v", err)
		return ModelMetadata{}, false
	}

	for _, key := range modelMetadataLookupKeys(model) {
		if meta, ok := byID[key]; ok {
			return meta, true
		}
	}
	r.LogOnce("missing:"+strings.ToLower(strings.TrimSpace(model)), "WARN: provider model metadata did not include %q; using conservative compaction defaults", model)
	return ModelMetadata{}, false
}

// metadataByID returns the cached metadata map, fetching it on first use and
// re-attempting failed fetches after compactionMetadataRetryCooldown.
func (r *CompactionMetadataResolver) metadataByID(ctx context.Context) (map[string]ModelMetadata, error) {
	r.fetchMu.Lock()
	defer r.fetchMu.Unlock()
	if r.fetched {
		return r.byID, r.lastErr
	}
	if r.session == nil {
		// No credentials will ever appear on this resolver; don't retry.
		r.fetched = true
		r.lastErr = fmt.Errorf("OpenAI auth session is required")
		return nil, r.lastErr
	}
	if r.lastErr != nil && time.Since(r.lastAttempt) < compactionMetadataRetryCooldown {
		return nil, r.lastErr
	}
	r.lastAttempt = time.Now()
	byID, err := FetchModelMetadataByID(ctx, r.baseURL, r.session)
	if err != nil {
		r.lastErr = err
		return nil, err
	}
	r.byID = byID
	r.lastErr = nil
	r.fetched = true
	return r.byID, nil
}

func (r *CompactionMetadataResolver) LogOnce(key, format string, args ...any) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.logged[key]; ok {
		return
	}
	r.logged[key] = struct{}{}
	log.Printf(format, args...)
}

func modelMetadataLookupKeys(model string) []string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return nil
	}
	keys := []string{model}
	_, unprefixed := agentsdk.ParseModelPrefix(model)
	unprefixed = strings.ToLower(strings.TrimSpace(unprefixed))
	if unprefixed != "" && unprefixed != model {
		keys = append(keys, unprefixed)
	}
	return keys
}

func CompactionDefaultsFromModelMetadata(meta ModelMetadata) (triggerTokens, targetTokens int, ok bool) {
	contextWindow := meta.ResolvedContextWindow()
	triggerTokens = meta.AutoCompactTokenLimit
	if contextWindow > 0 {
		contextLimit := (contextWindow * 9) / 10
		if triggerTokens <= 0 || triggerTokens > contextLimit {
			triggerTokens = contextLimit
		}
		targetTokens = contextWindow / 2
	} else if triggerTokens > 0 {
		targetTokens = triggerTokens / 2
	}
	if triggerTokens <= 0 {
		return 0, 0, false
	}
	if targetTokens <= 0 || targetTokens >= triggerTokens {
		targetTokens = triggerTokens / 2
	}
	return triggerTokens, targetTokens, true
}

// CompactionConfigFromMetadata builds a compaction config using provider model
// metadata. It returns ok=false when metadata is unavailable or incomplete.
// Authentication uses OPENAI_API_KEY when set; prefer
// CompactionConfigFromMetadataWithAuthSession to supply explicit credentials.
func CompactionConfigFromMetadata(ctx context.Context, baseURL, model string) (agentsdk.CompactionConfig, bool) {
	resolver := NewCompactionMetadataResolver(baseURL, NewAPIKeyAuthSession(os.Getenv("OPENAI_API_KEY")))
	return compactionConfigFromResolver(ctx, resolver, model)
}

// CompactionConfigFromMetadataWithAuthSession builds a compaction config using
// provider model metadata fetched with an explicit auth session.
func CompactionConfigFromMetadataWithAuthSession(ctx context.Context, baseURL, model string, session *AuthSession) (agentsdk.CompactionConfig, bool) {
	resolver := NewCompactionMetadataResolver(baseURL, session)
	return compactionConfigFromResolver(ctx, resolver, model)
}

func compactionConfigFromResolver(ctx context.Context, resolver *CompactionMetadataResolver, model string) (agentsdk.CompactionConfig, bool) {
	meta, ok := resolver.Lookup(ctx, model)
	if !ok {
		return agentsdk.CompactionConfig{}, false
	}
	trigger, target, ok := CompactionDefaultsFromModelMetadata(meta)
	if !ok {
		return agentsdk.CompactionConfig{}, false
	}
	cfg := agentsdk.DefaultCompactionConfig()
	cfg.TriggerTokens = trigger
	cfg.TargetTokens = target
	return cfg, true
}
