package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory represents a single stored memory entry.
type Memory struct {
	ID         uuid.UUID       `json:"id"`
	Namespace  string          `json:"namespace"`
	Content    string          `json:"content"`
	Tags       []string        `json:"tags"`
	SourceRun  string          `json:"source_run"`
	Metadata   json.RawMessage `json:"metadata"`
	Similarity float64         `json:"similarity,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Embedder produces vector embeddings from text.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Store persists and retrieves semantic memories.
type Store interface {
	Store(ctx context.Context, namespace, content string, tags []string, sourceRun string, metadata json.RawMessage) (*Memory, error)
	Search(ctx context.Context, namespace, query string, tags []string, limit int) ([]Memory, error)
	List(ctx context.Context, namespace string, tags []string, limit int) ([]Memory, error)
	Delete(ctx context.Context, namespace string, id uuid.UUID) error
}

// InMemoryStore is a small process-local Store implementation for SDK clients
// that do not need durable storage. Search uses simple substring keyword
// matching, not vector similarity — for true semantic search, plug in a Store
// backed by a vector database.
type InMemoryStore struct {
	mu       sync.RWMutex
	memories []Memory
}

// NewInMemoryStore returns an empty in-memory memory store.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{}
}

func (s *InMemoryStore) Store(_ context.Context, namespace, content string, tags []string, sourceRun string, metadata json.RawMessage) (*Memory, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if content == "" {
		return nil, fmt.Errorf("content is required")
	}
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = json.RawMessage(`{}`)
	}

	mem := Memory{
		ID:        uuid.New(),
		Namespace: namespace,
		Content:   content,
		Tags:      append([]string(nil), tags...),
		SourceRun: sourceRun,
		Metadata:  append(json.RawMessage(nil), metadata...),
		CreatedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	s.memories = append(s.memories, mem)
	s.mu.Unlock()

	out := mem
	return &out, nil
}

func (s *InMemoryStore) Search(_ context.Context, namespace, query string, tags []string, limit int) ([]Memory, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if limit <= 0 {
		limit = 10
	}

	query = strings.ToLower(query)
	out := s.filter(namespace, tags)
	scored := out[:0]
	for _, mem := range out {
		content := strings.ToLower(mem.Content)
		if strings.Contains(content, query) {
			mem.Similarity = 1
			scored = append(scored, mem)
			continue
		}
		for _, term := range strings.Fields(query) {
			if strings.Contains(content, term) {
				mem.Similarity += 0.25
			}
		}
		if mem.Similarity > 0 {
			scored = append(scored, mem)
		}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Similarity == scored[j].Similarity {
			return scored[i].CreatedAt.After(scored[j].CreatedAt)
		}
		return scored[i].Similarity > scored[j].Similarity
	})
	return limitMemories(scored, limit), nil
}

func (s *InMemoryStore) List(_ context.Context, namespace string, tags []string, limit int) ([]Memory, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if limit <= 0 {
		limit = 50
	}
	out := s.filter(namespace, tags)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return limitMemories(out, limit), nil
}

func (s *InMemoryStore) Delete(_ context.Context, namespace string, id uuid.UUID) error {
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i, mem := range s.memories {
		if mem.Namespace == namespace && mem.ID == id {
			s.memories = append(s.memories[:i], s.memories[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("memory not found")
}

func (s *InMemoryStore) filter(namespace string, tags []string) []Memory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Memory, 0, len(s.memories))
	for _, mem := range s.memories {
		if mem.Namespace != namespace {
			continue
		}
		if len(tags) > 0 && !hasAnyTag(mem.Tags, tags) {
			continue
		}
		out = append(out, cloneMemory(mem))
	}
	return out
}

func hasAnyTag(actual, wanted []string) bool {
	set := make(map[string]struct{}, len(actual))
	for _, tag := range actual {
		set[tag] = struct{}{}
	}
	for _, tag := range wanted {
		if _, ok := set[tag]; ok {
			return true
		}
	}
	return false
}

func cloneMemory(mem Memory) Memory {
	mem.Tags = append([]string(nil), mem.Tags...)
	mem.Metadata = append(json.RawMessage(nil), mem.Metadata...)
	return mem
}

func limitMemories(memories []Memory, limit int) []Memory {
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}
	out := make([]Memory, len(memories))
	copy(out, memories)
	return out
}

// NoopEmbedder returns all-zero embeddings and is useful for local examples.
type NoopEmbedder struct {
	Dimension int
}

func (e *NoopEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	dim := e.Dimension
	if dim <= 0 {
		dim = 1536
	}
	return make([]float32, dim), nil
}

// VectorLiteral converts a vector to the pgvector literal format.
func VectorLiteral(v []float32) string {
	if len(v) == 0 {
		return "[]"
	}
	parts := make([]string, len(v))
	for i, f := range v {
		parts[i] = fmt.Sprintf("%g", f)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
