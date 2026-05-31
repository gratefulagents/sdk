# Postgres / pgvector Memory Store

The SDK memory tool accepts any implementation of `memory.Store`. This example
shows a Postgres store that uses pgvector for semantic search. It lives in docs
instead of a default Go test because it requires an application database, the
pgvector extension, and the `pgx` driver.

Install the application dependency in your app, not in the SDK:

```sh
go get github.com/jackc/pgx/v5
```

## Schema

The vector dimension must match your embedder. The SDK's default
`memory.NewOpenAIEmbedder(..., "", "")` model is `text-embedding-3-small`,
which returns 1536 dimensions.

```sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE IF NOT EXISTS agent_memories (
  id uuid PRIMARY KEY,
  namespace text NOT NULL,
  content text NOT NULL,
  tags text[] NOT NULL DEFAULT '{}',
  source_run text NOT NULL DEFAULT '',
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  embedding vector(1536) NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS agent_memories_namespace_created_idx
  ON agent_memories (namespace, created_at DESC);

CREATE INDEX IF NOT EXISTS agent_memories_tags_idx
  ON agent_memories USING gin (tags);

CREATE INDEX IF NOT EXISTS agent_memories_embedding_idx
  ON agent_memories USING hnsw (embedding vector_cosine_ops);
```

## Store Implementation

```go
package memorypostgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gratefulagents/sdk/pkg/agentsdk/memory"
)

type PostgresStore struct {
	db       *pgxpool.Pool
	embedder memory.Embedder
}

func NewPostgresStore(db *pgxpool.Pool, embedder memory.Embedder) *PostgresStore {
	return &PostgresStore{db: db, embedder: embedder}
}

func (s *PostgresStore) Store(ctx context.Context, namespace, content string, tags []string, sourceRun string, metadata json.RawMessage) (*memory.Memory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres memory store is not configured")
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("embedder is required")
	}
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

	vector, err := s.embedder.Embed(ctx, content)
	if err != nil {
		return nil, fmt.Errorf("embed memory content: %w", err)
	}

	row := s.db.QueryRow(ctx, `
		INSERT INTO agent_memories (id, namespace, content, tags, source_run, metadata, embedding)
		VALUES ($1, $2, $3, $4::text[], $5, $6::jsonb, $7::vector)
		RETURNING id, namespace, content, tags, source_run, metadata, 0::double precision, created_at
	`, uuid.New(), namespace, content, tags, sourceRun, string(metadata), memory.VectorLiteral(vector))

	return scanMemory(row)
}

func (s *PostgresStore) Search(ctx context.Context, namespace, query string, tags []string, limit int) ([]memory.Memory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres memory store is not configured")
	}
	if s.embedder == nil {
		return nil, fmt.Errorf("embedder is required")
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if query == "" {
		return nil, fmt.Errorf("query is required")
	}
	if limit <= 0 {
		limit = 10
	}

	vector, err := s.embedder.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed memory query: %w", err)
	}

	rows, err := s.db.Query(ctx, `
		SELECT
			id,
			namespace,
			content,
			tags,
			source_run,
			metadata,
			1 - (embedding <=> $4::vector) AS similarity,
			created_at
		FROM agent_memories
		WHERE namespace = $1
		  AND ($2 OR tags && $3::text[])
		ORDER BY embedding <=> $4::vector
		LIMIT $5
	`, namespace, len(tags) == 0, tags, memory.VectorLiteral(vector), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMemories(rows)
}

func (s *PostgresStore) List(ctx context.Context, namespace string, tags []string, limit int) ([]memory.Memory, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("postgres memory store is not configured")
	}
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required")
	}
	if limit <= 0 {
		limit = 50
	}

	rows, err := s.db.Query(ctx, `
		SELECT
			id,
			namespace,
			content,
			tags,
			source_run,
			metadata,
			0::double precision AS similarity,
			created_at
		FROM agent_memories
		WHERE namespace = $1
		  AND ($2 OR tags && $3::text[])
		ORDER BY created_at DESC
		LIMIT $4
	`, namespace, len(tags) == 0, tags, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMemories(rows)
}

func (s *PostgresStore) Delete(ctx context.Context, namespace string, id uuid.UUID) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("postgres memory store is not configured")
	}
	if namespace == "" {
		return fmt.Errorf("namespace is required")
	}

	tag, err := s.db.Exec(ctx, `
		DELETE FROM agent_memories
		WHERE namespace = $1 AND id = $2
	`, namespace, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("memory not found")
	}
	return nil
}

type memoryRow interface {
	Scan(dest ...any) error
}

type memoryRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanMemory(row memoryRow) (*memory.Memory, error) {
	var mem memory.Memory
	if err := row.Scan(
		&mem.ID,
		&mem.Namespace,
		&mem.Content,
		&mem.Tags,
		&mem.SourceRun,
		&mem.Metadata,
		&mem.Similarity,
		&mem.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &mem, nil
}

func scanMemories(rows memoryRows) ([]memory.Memory, error) {
	var memories []memory.Memory
	for rows.Next() {
		mem, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		memories = append(memories, *mem)
	}
	return memories, rows.Err()
}
```

## Register The Store

Use the default registry when you build tools yourself:

```go
pool, err := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
if err != nil {
	return err
}
defer pool.Close()

auth := openai.NewAPIKeyAuthSession(os.Getenv("OPENAI_API_KEY"))
embedder := memory.NewOpenAIEmbedder(auth, "", "")
store := memorypostgres.NewPostgresStore(pool, embedder)

registry := tools.NewRegistry(
	workDir,
	tools.WithMemoryStore(store, "acme/repo", runID, "https://github.com/acme/repo"),
)
agent.Tools = registry.Tools()
```

Use `ExtraTools` when you use `runtime.Builder`, because `runtime.Config` does
not currently have a first-class memory-store field:

```go
memoryTool := memorytool.New(store, "acme/repo", runID, "https://github.com/acme/repo")

bundle, err := runtime.NewBuilder(runtime.Config{
	WorkDir:     workDir,
	EnableTools: true,
	ExtraTools:  []agentsdk.Tool{memoryTool},
}).Build(ctx)
```

## Other Stores

Postgres is not special to the SDK. A Redis, SQLite, Qdrant, Pinecone, or
application-native store uses the same four methods:

```go
type Store interface {
	Store(ctx context.Context, namespace, content string, tags []string, sourceRun string, metadata json.RawMessage) (*memory.Memory, error)
	Search(ctx context.Context, namespace, query string, tags []string, limit int) ([]memory.Memory, error)
	List(ctx context.Context, namespace string, tags []string, limit int) ([]memory.Memory, error)
	Delete(ctx context.Context, namespace string, id uuid.UUID) error
}
```

For production stores, keep namespace isolation mandatory, honor tag filters and
limits, respect `context.Context`, and use an embedder that matches your vector
index dimension.
