package projectstate

import (
	"context"
	"math"
	"sort"
	"time"
)

// Embedder turns text into dense vectors for semantic memory retrieval. It is
// optional: without an embedder, SearchMemories ranks by LexicalScore alone.
//
// Implementations should be safe for concurrent use and should embed the input
// slice as a batch, returning one vector per input in the same order.
type Embedder interface {
	// Embed returns one vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Model identifies the embedding model. It is persisted alongside cached
	// vectors so the cache can be invalidated when the model changes.
	Model() string
}

// HybridConfig tunes how lexical and semantic signals are fused during recall.
// The zero value is not usable on its own; callers should start from
// DefaultHybridConfig and override individual fields.
type HybridConfig struct {
	// LexicalWeight weights the normalized lexical (keyword) score.
	LexicalWeight float64
	// DenseWeight weights the semantic (cosine similarity) score.
	DenseWeight float64
	// PinnedBoost is added to the score of preference and decision memories
	// so durable guidance surfaces ahead of incidental matches of equal
	// relevance.
	PinnedBoost float64
	// RecencyWeight weights an exponential recency score in [0,1].
	RecencyWeight float64
	// RecencyHalfLife is the age at which the recency score decays to 0.5.
	RecencyHalfLife time.Duration
	// MinScore drops candidates whose fused score is below this threshold.
	// Keep at 0 to preserve every filtered candidate.
	MinScore float64
}

// DefaultHybridConfig returns balanced defaults: semantic similarity leads,
// keyword overlap supports it, preferences and decisions get a small boost, and recency
// is a light tie-breaker with a 30 day half-life.
func DefaultHybridConfig() HybridConfig {
	return HybridConfig{
		LexicalWeight:   0.4,
		DenseWeight:     0.6,
		PinnedBoost:     0.15,
		RecencyWeight:   0.1,
		RecencyHalfLife: 30 * 24 * time.Hour,
		MinScore:        0,
	}
}

func (c HybridConfig) normalized() HybridConfig {
	if c.LexicalWeight == 0 && c.DenseWeight == 0 {
		c.LexicalWeight = DefaultHybridConfig().LexicalWeight
		c.DenseWeight = DefaultHybridConfig().DenseWeight
	}
	if c.RecencyHalfLife <= 0 {
		c.RecencyHalfLife = DefaultHybridConfig().RecencyHalfLife
	}
	return c
}

// rankHybrid scores candidates against query, best first, dropping those with
// no relevance signal.
//
// Without a query vector (no embedder, or the query embedding failed) the score
// is LexicalScore alone. With one, the score fuses the lexical score with
// cosine similarity against the cached vectors, plus the kind boost and
// recency weight from cfg. Memories without a cached vector score 0 on the
// dense axis.
func rankHybrid(query string, candidates []Memory, queryVec []float32, vectors map[string][]float32, cfg HybridConfig, now time.Time) []MemoryHit {
	cfg = cfg.normalized()
	hits := make([]MemoryHit, 0, len(candidates))
	for _, mem := range candidates {
		lex := LexicalScore(query, mem)
		score := lex
		if len(queryVec) > 0 {
			var dense float64
			if vec, ok := vectors[mem.ID]; ok && len(vec) > 0 {
				dense = math.Max(0, cosineSimilarity(queryVec, vec))
			}
			// Boosts amplify relevance; they must not manufacture it.
			if lex == 0 && dense == 0 {
				continue
			}
			score = cfg.LexicalWeight*lex + cfg.DenseWeight*dense
			if mem.Kind == MemoryKindPreference || mem.Kind == MemoryKindDecision {
				score += cfg.PinnedBoost
			}
			if cfg.RecencyWeight > 0 {
				score += cfg.RecencyWeight * recencyScore(mem.UpdatedAt, now, cfg.RecencyHalfLife)
			}
			if score < cfg.MinScore {
				continue
			}
		}
		if score <= 0 {
			continue
		}
		hits = append(hits, MemoryHit{Memory: cloneMemory(mem), Score: score})
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		if !hits[i].UpdatedAt.Equal(hits[j].UpdatedAt) {
			return hits[i].UpdatedAt.After(hits[j].UpdatedAt)
		}
		return hits[i].ID < hits[j].ID
	})
	return hits
}

// recencyScore returns an exponential decay in (0,1]: 1 at age 0, 0.5 at one
// half-life, approaching 0 for old memories.
func recencyScore(updatedAt, now time.Time, halfLife time.Duration) float64 {
	if updatedAt.IsZero() || halfLife <= 0 {
		return 0
	}
	age := now.Sub(updatedAt)
	if age <= 0 {
		return 1
	}
	return math.Pow(0.5, float64(age)/float64(halfLife))
}

// cosineSimilarity returns the cosine of the angle between two vectors in
// [-1,1]. Mismatched or zero-length vectors yield 0.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		av, bv := float64(a[i]), float64(b[i])
		dot += av * bv
		na += av * av
		nb += bv * bv
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
