// Package projectstate implements durable, event-sourced project state for
// agents: typed tasks, typed citable memories, and a deterministic briefing.
// State is persisted as an append-only event log (events.jsonl for
// FilesystemStore, an events table for SQLiteStore) and rebuilt by replay, so
// it survives process restarts and context compaction.
//
// # Memory model (v2)
//
// A Memory is a short, typed, citable piece of durable knowledge: a one-line
// Title (≤ MaxMemoryTitleLen) and a Body (≤ MaxMemoryBodyLen). Kinds are:
//
//   - preference: how the user/owner wants work done
//   - decision: durable product/architecture decisions with rationale
//   - fact: non-obvious facts and gotchas about the project
//   - procedure: repeatable how-tos
//
// Citations anchor a memory to workspace-relative file paths or URLs, and
// CommitSHA records the repository HEAD when it was last saved or verified, so
// callers can detect staleness (cited files changed or removed, or VerifiedAt
// older than MemoryStaleAfter) and re-verify with VerifyMemory. TouchMemories
// records reads; usage feeds briefing selection.
//
// SaveMemory validates input with ValidateMemoryInput, creates (empty ID) or
// fully replaces (existing ID) a memory, and enforces DefaultMemoryCap so the
// set is consolidated rather than accumulated. Legacy v1 memory.upserted
// events are replayed into v2 memories (title derived with LegacyMemoryTitle,
// kinds mapped with NormalizeMemoryKind); v1 session summaries are ignored.
//
// # Recall
//
// SearchMemories ranks memories by LexicalScore, the weighted fraction of
// query tokens found in the title (1.0) or body/citations (0.6). When a store
// is configured with an Embedder, the lexical score is fused with cosine
// similarity over cached embeddings of Title+"\n"+Body, plus a small boost for
// preferences/decisions and a recency weight (tunable via HybridConfig).
// Embeddings are cached on write keyed by content hash and model, backfilled
// lazily on search, and failures never block writes; recall degrades to the
// lexical signal.
//
// # Briefing
//
// PrimeContext renders RenderBriefing: the active task, ready and blocked
// work, and a memory index (id, kind, title) chosen by SelectBriefingMemories
// within BriefingMemoryBudget bytes. Output contains no timestamps and is
// deterministic for a given state, so it is prompt-cache friendly. Agents load
// full memories on demand by id and search for anything not listed.
//
// The exported pure helpers (NormalizeMemoryKind, ValidateMemoryInput,
// LexicalScore, Similarity, SelectBriefingMemories, RenderBriefing, the task
// helpers, ...) are shared with other Store implementations so behavior stays
// identical across backends.
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
