// Package projectstate implements durable, event-sourced project state for
// agents: typed tasks, typed long-term memories, session summaries, and a
// prime-context builder. State is persisted under a state directory as an
// append-only event log (events.jsonl) with derived JSON indexes, so it
// survives process restarts and context compaction.
//
// # Memory model
//
// Memories are typed by Kind (pinned, semantic, episodic, procedural) and
// Scope (project, user, task, file). Store them with UpsertMemory, retrieve
// them with SearchMemories (query required) or ListMemories, and surface a
// compact session-start summary with PrimeContext.
//
// # Hybrid recall
//
// By default SearchMemories ranks memories with a lexical keyword score. When
// a FilesystemStore is configured with an Embedder, recall becomes hybrid: it
// fuses the normalized lexical score with cosine similarity over cached
// embeddings, plus a small pinned boost and recency decay (all tunable via
// HybridConfig). Candidates are filtered by kind and tags but not by keyword,
// so semantically relevant memories surface even when they share no exact
// terms with the query. With no Embedder, recall falls back to the lexical
// path unchanged, so embeddings are fully optional and backward compatible.
//
// Embeddings are computed on write and cached in indexes/embeddings.json,
// keyed by content hash and model so the cache self-invalidates when a
// memory's content or the embedding model changes. Memories written before an
// embedder was configured are backfilled lazily on the next recall. Embedding
// failures never block writes and degrade recall to whatever vectors already
// exist alongside the lexical signal.
//
// # Embedders
//
// OpenAIEmbedder implements Embedder against any OpenAI-compatible
// /v1/embeddings endpoint. Set OpenAIEmbedderOptions.BaseURL to choose the
// provider:
//
//   - OpenAI: "https://api.openai.com/v1" with "text-embedding-3-small".
//   - Local (Ollama): "http://localhost:11434/v1" with e.g. "bge-m3".
//
// OpenRouter currently exposes chat/completions but not a general-purpose
// /v1/embeddings endpoint, so it cannot back embeddings today; use OpenAI or a
// local model for vectors. If OpenRouter adds an embeddings route, point
// BaseURL at it with no code change. To use a different backend entirely,
// implement the Embedder interface.
package projectstate
