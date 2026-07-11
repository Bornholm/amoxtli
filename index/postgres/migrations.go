package postgres

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

// DefaultVectorSize is the default dimension of the pgvector column,
// consistent with the sqlitevec backend.
const DefaultVectorSize int = 768

func migrations(vectorSize int) []string {
	return []string{
		`CREATE EXTENSION IF NOT EXISTS vector;`,
		`CREATE EXTENSION IF NOT EXISTS unaccent;`,
		fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS amoxtli_chunks (
				id BIGSERIAL PRIMARY KEY,
				source TEXT NOT NULL,
				section_id TEXT NOT NULL,
				chunk_index INTEGER NOT NULL DEFAULT 0,
				content TEXT NOT NULL,
				lang TEXT NOT NULL DEFAULT 'simple',
				tsv TSVECTOR NOT NULL,
				embedding VECTOR(%d)
			);
		`, vectorSize),
		`CREATE INDEX IF NOT EXISTS amoxtli_chunks_source_idx ON amoxtli_chunks (source);`,
		`CREATE INDEX IF NOT EXISTS amoxtli_chunks_section_idx ON amoxtli_chunks (section_id);`,
		`CREATE INDEX IF NOT EXISTS amoxtli_chunks_tsv_idx ON amoxtli_chunks USING GIN (tsv);`,
		`CREATE INDEX IF NOT EXISTS amoxtli_chunks_embedding_idx ON amoxtli_chunks USING hnsw (embedding vector_cosine_ops);`,
		`
			CREATE TABLE IF NOT EXISTS amoxtli_chunk_collections (
				chunk_id BIGINT NOT NULL REFERENCES amoxtli_chunks (id) ON DELETE CASCADE,
				collection_id TEXT NOT NULL,
				PRIMARY KEY (chunk_id, collection_id)
			);
		`,
		`CREATE INDEX IF NOT EXISTS amoxtli_chunk_collections_collection_idx ON amoxtli_chunk_collections (collection_id);`,
	}
}

func createGetPool(pool *pgxpool.Pool, migrations []string) func(ctx context.Context) (*pgxpool.Pool, error) {
	var (
		migrateOnce sync.Once
		migrateErr  error
	)

	return func(ctx context.Context) (*pgxpool.Pool, error) {
		migrateOnce.Do(func() {
			for _, sql := range migrations {
				if _, err := pool.Exec(ctx, sql); err != nil {
					migrateErr = errors.Wrapf(err, "could not execute migration '%s'", sql)
					return
				}
			}
		})
		if migrateErr != nil {
			return nil, errors.WithStack(migrateErr)
		}

		return pool, nil
	}
}
