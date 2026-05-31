package openai

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/gratefulagents/sdk/pkg/agentsdk"
)

// CompactionMetadataResolver fetches provider model metadata once and uses it
// to derive SDK compaction defaults for OpenAI-compatible models.
type CompactionMetadataResolver struct {
	baseURL string
	session *AuthSession

	once sync.Once
	byID map[string]ModelMetadata
	err  error

	mu     sync.Mutex
	logged map[string]struct{}
}

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
	r.once.Do(func() {
		if r.session == nil {
			r.err = fmt.Errorf("OpenAI auth session is required")
			return
		}
		r.byID, r.err = FetchModelMetadataByID(ctx, r.baseURL, r.session)
	})
	if r.err != nil {
		r.LogOnce("fetch-error", "WARN: failed to fetch OpenAI model metadata for compaction: %v", r.err)
		return ModelMetadata{}, false
	}

	for _, key := range modelMetadataLookupKeys(model) {
		if meta, ok := r.byID[key]; ok {
			return meta, true
		}
	}
	r.LogOnce("missing:"+strings.ToLower(strings.TrimSpace(model)), "WARN: provider model metadata did not include %q; using conservative compaction defaults", model)
	return ModelMetadata{}, false
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
func CompactionConfigFromMetadata(ctx context.Context, baseURL, model string) (agentsdk.CompactionConfig, bool) {
	resolver := NewCompactionMetadataResolver(baseURL)
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
