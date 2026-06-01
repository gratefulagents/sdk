package projectstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const embeddingsFileName = "embeddings.json"

// embeddingRecord is a cached vector for one memory. Hash and Model let the
// cache invalidate itself when the memory content or the embedding model
// changes, so vectors never silently go stale.
type embeddingRecord struct {
	Hash   string    `json:"hash"`
	Model  string    `json:"model"`
	Dims   int       `json:"dims"`
	Vector []float32 `json:"vector"`
}

type embeddingCache struct {
	SchemaVersion int                        `json:"schema_version"`
	Model         string                     `json:"model"`
	UpdatedAt     time.Time                  `json:"updated_at"`
	Vectors       map[string]embeddingRecord `json:"vectors"`
}

func hashContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (r embeddingRecord) fresh(content, model string) bool {
	return len(r.Vector) > 0 && r.Model == model && r.Hash == hashContent(content)
}

// readEmbeddings loads the vector cache from indexes/embeddings.json. A missing
// file yields an empty cache rather than an error.
func (s *FilesystemStore) readEmbeddings() (map[string]embeddingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.stateDir, "indexes", embeddingsFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]embeddingRecord{}, nil
		}
		return nil, err
	}
	var cache embeddingCache
	if err := json.Unmarshal(data, &cache); err != nil {
		// A corrupt derived cache should not break recall; rebuild lazily.
		return map[string]embeddingRecord{}, nil
	}
	if cache.Vectors == nil {
		cache.Vectors = map[string]embeddingRecord{}
	}
	return cache.Vectors, nil
}

// writeEmbeddings atomically persists the vector cache.
func (s *FilesystemStore) writeEmbeddings(records map[string]embeddingRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	model := ""
	if s.embedder != nil {
		model = s.embedder.Model()
	}
	cache := embeddingCache{
		SchemaVersion: SchemaVersion,
		Model:         model,
		UpdatedAt:     time.Now().UTC(),
		Vectors:       records,
	}
	return writeJSONAtomic(filepath.Join(s.stateDir, "indexes", embeddingsFileName), cache)
}

// cacheMemoryEmbedding embeds a single memory's content and persists it. It is
// best-effort: embedding failures are returned but callers on the write path
// ignore them so storing a memory never fails because the embedder is down.
func (s *FilesystemStore) cacheMemoryEmbedding(ctx context.Context, id, content string) error {
	if s.embedder == nil || id == "" || content == "" {
		return nil
	}
	records, err := s.readEmbeddings()
	if err != nil {
		return err
	}
	if rec, ok := records[id]; ok && rec.fresh(content, s.embedder.Model()) {
		return nil
	}
	vecs, err := s.embedder.Embed(ctx, []string{content})
	if err != nil {
		return err
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		return nil
	}
	records[id] = embeddingRecord{
		Hash:   hashContent(content),
		Model:  s.embedder.Model(),
		Dims:   len(vecs[0]),
		Vector: vecs[0],
	}
	return s.writeEmbeddings(records)
}

// ensureEmbeddings returns a memoryID -> vector map for the given memories,
// embedding any that are missing or stale and persisting the refreshed cache.
// Backfill failures degrade gracefully: whatever vectors already exist are
// returned so recall can still use them alongside the lexical signal.
func (s *FilesystemStore) ensureEmbeddings(ctx context.Context, memories []Memory) map[string][]float32 {
	vectors := map[string][]float32{}
	if s.embedder == nil {
		return vectors
	}
	records, err := s.readEmbeddings()
	if err != nil {
		records = map[string]embeddingRecord{}
	}
	model := s.embedder.Model()

	var missingIDs []string
	var missingText []string
	for _, mem := range memories {
		if rec, ok := records[mem.ID]; ok && rec.fresh(mem.Content, model) {
			vectors[mem.ID] = rec.Vector
			continue
		}
		if mem.Content == "" {
			continue
		}
		missingIDs = append(missingIDs, mem.ID)
		missingText = append(missingText, mem.Content)
	}

	if len(missingText) == 0 {
		return vectors
	}
	vecs, err := s.embedder.Embed(ctx, missingText)
	if err != nil || len(vecs) != len(missingText) {
		// Backfill failed; return whatever we already had cached.
		return vectors
	}
	for i, id := range missingIDs {
		if len(vecs[i]) == 0 {
			continue
		}
		records[id] = embeddingRecord{
			Hash:   hashContent(missingText[i]),
			Model:  model,
			Dims:   len(vecs[i]),
			Vector: vecs[i],
		}
		vectors[id] = vecs[i]
	}
	_ = s.writeEmbeddings(records)
	return vectors
}
