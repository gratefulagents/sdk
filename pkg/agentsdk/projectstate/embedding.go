package projectstate

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"
)

// Embedder turns text into dense vectors for semantic memory retrieval. It is
// optional: when a FilesystemStore has no embedder, memory recall falls back to
// the lexical keyword search and behaves exactly as before.
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
	// PinnedBoost is added to the score of pinned memories so durable facts
	// surface ahead of incidental matches of equal relevance.
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
// keyword overlap supports it, pinned memories get a small boost, and recency
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

// scoredMemory pairs a memory with its fused relevance score.
type scoredMemory struct {
	memory Memory
	score  float64
}

// rankHybrid fuses lexical and semantic signals into a ranked memory list.
//
// queryVec may be nil (no embedder or embedding failure); in that case only the
// lexical signal contributes, so the function degrades gracefully to keyword
// ranking instead of failing the recall. vectors maps memory ID to its cached
// embedding; memories without a vector simply score 0 on the dense axis.
func rankHybrid(query string, candidates []Memory, queryVec []float32, vectors map[string][]float32, cfg HybridConfig, now time.Time) []Memory {
	cfg = cfg.normalized()
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))

	lexRaw := make([]float64, len(candidates))
	var maxLex float64
	for i, mem := range candidates {
		lexRaw[i] = lexicalScore(mem, terms)
		if lexRaw[i] > maxLex {
			maxLex = lexRaw[i]
		}
	}

	scored := make([]scoredMemory, 0, len(candidates))
	for i, mem := range candidates {
		var lexNorm float64
		if maxLex > 0 {
			lexNorm = lexRaw[i] / maxLex
		}
		var denseNorm float64
		if len(queryVec) > 0 {
			if vec, ok := vectors[mem.ID]; ok && len(vec) > 0 {
				denseNorm = math.Max(0, cosineSimilarity(queryVec, vec))
			}
		}
		score := cfg.LexicalWeight*lexNorm + cfg.DenseWeight*denseNorm
		if mem.Kind == MemoryKindPinned {
			score += cfg.PinnedBoost
		}
		if cfg.RecencyWeight > 0 {
			score += cfg.RecencyWeight * recencyScore(mem.UpdatedAt, now, cfg.RecencyHalfLife)
		}
		if score < cfg.MinScore {
			continue
		}
		scored = append(scored, scoredMemory{memory: cloneMemory(mem), score: score})
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].memory.UpdatedAt.After(scored[j].memory.UpdatedAt)
	})

	out := make([]Memory, len(scored))
	for i := range scored {
		out[i] = scored[i].memory
	}
	return out
}

// lexicalScore ranks a memory against query terms using term frequency plus a
// coverage bonus for matching distinct terms. It is deliberately simple and
// deterministic so hybrid ranking stays testable without a live embedder.
func lexicalScore(mem Memory, terms []string) float64 {
	if len(terms) == 0 {
		return 0
	}
	haystack := strings.ToLower(strings.Join(append([]string{mem.Content, mem.Kind, mem.Scope}, append(mem.Tags, append(mem.TaskIDs, mem.FilePaths...)...)...), " "))
	if haystack == "" {
		return 0
	}
	var tf float64
	var covered int
	for _, term := range terms {
		if term == "" {
			continue
		}
		if n := strings.Count(haystack, term); n > 0 {
			tf += float64(n)
			covered++
		}
	}
	if covered == 0 {
		return 0
	}
	// Coverage of distinct query terms is weighted more heavily than raw term
	// frequency so a memory matching every term outranks one that merely
	// repeats a single term.
	return tf + 2*float64(covered)
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
