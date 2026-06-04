package projectstate

import (
	"crypto/sha256"
	"encoding/hex"
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
