package postgres

import (
	"context"
	"encoding/gob"
	"io"

	"github.com/bornholm/amoxtli/backup"
	"github.com/pkg/errors"
)

func init() {
	gob.Register(SnapshottedRecord{})
	gob.Register(SnapshottedMetadata{})
}

type SnapshottedMetadata struct {
	Model      string
	VectorSize int
}

type SnapshottedRecord struct {
	Source     string
	SectionID  string
	ChunkIndex int
	Content    string
	Lang       string
	// Embedding is the pgvector text literal ("[0.1,...]"), empty when the
	// chunk has no embedding.
	Embedding   string
	Collections []string
}

// GenerateSnapshot implements backup.Snapshotable.
func (i *Index) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	pool, err := i.getPool(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	r, w := io.Pipe()

	go func() {
		defer w.Close()

		encoder := gob.NewEncoder(w)

		metadata := SnapshottedMetadata{
			Model:      i.model,
			VectorSize: i.vectorSize,
		}

		if err := encoder.Encode(metadata); err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}

		rows, err := pool.Query(ctx, `
			SELECT
				c.source,
				c.section_id,
				c.chunk_index,
				c.content,
				c.lang,
				COALESCE(c.embedding::text, ''),
				COALESCE(array_agg(cc.collection_id) FILTER (WHERE cc.collection_id IS NOT NULL), '{}')
			FROM amoxtli_chunks c
			LEFT JOIN amoxtli_chunk_collections cc ON cc.chunk_id = c.id
			GROUP BY c.id
			ORDER BY c.id;
		`)
		if err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}
		defer rows.Close()

		for rows.Next() {
			record := SnapshottedRecord{}
			if err := rows.Scan(&record.Source, &record.SectionID, &record.ChunkIndex, &record.Content, &record.Lang, &record.Embedding, &record.Collections); err != nil {
				w.CloseWithError(errors.WithStack(err))
				return
			}

			if err := encoder.Encode(record); err != nil {
				w.CloseWithError(errors.WithStack(err))
				return
			}
		}

		if err := rows.Err(); err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}
	}()

	return io.NopCloser(r), nil
}

// RestoreSnapshot implements backup.Snapshotable.
func (i *Index) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	decoder := gob.NewDecoder(r)

	metadata := SnapshottedMetadata{}

	if err := decoder.Decode(&metadata); err != nil {
		return errors.WithStack(err)
	}

	if metadata.Model != i.model {
		return errors.Errorf("could not restore snapshot with a different embedding model '%s'", metadata.Model)
	}

	if metadata.VectorSize != i.vectorSize {
		return errors.Errorf("could not restore snapshot with a different vector size %d (expected %d)", metadata.VectorSize, i.vectorSize)
	}

	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	if _, err := pool.Exec(ctx, `TRUNCATE amoxtli_chunks CASCADE;`); err != nil {
		return errors.WithStack(err)
	}

	batchSize := 1000
	batch := make([]*SnapshottedRecord, 0, batchSize)

	for {
		var record SnapshottedRecord
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				if len(batch) > 0 {
					if err := i.restoreRecords(ctx, batch...); err != nil {
						return errors.WithStack(err)
					}
				}

				return nil
			}

			return errors.WithStack(err)
		}

		batch = append(batch, &record)

		if len(batch) >= batchSize {
			if err := i.restoreRecords(ctx, batch...); err != nil {
				return errors.WithStack(err)
			}

			batch = batch[:0]
		}
	}
}

func (i *Index) restoreRecords(ctx context.Context, records ...*SnapshottedRecord) error {
	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	defer tx.Rollback(ctx)

	for _, record := range records {
		var chunkID int64
		err := tx.QueryRow(ctx, `
			INSERT INTO amoxtli_chunks (source, section_id, chunk_index, content, lang, tsv, embedding)
			VALUES ($1, $2, $3, $4, $5,
				to_tsvector($5::text::regconfig, unaccent($4)) || to_tsvector('simple', unaccent($4)),
				NULLIF($6, '')::vector)
			RETURNING id;
		`, record.Source, record.SectionID, record.ChunkIndex, record.Content, record.Lang, record.Embedding).Scan(&chunkID)
		if err != nil {
			return errors.WithStack(err)
		}

		for _, collectionID := range record.Collections {
			_, err := tx.Exec(ctx, `
				INSERT INTO amoxtli_chunk_collections (chunk_id, collection_id)
				VALUES ($1, $2)
				ON CONFLICT DO NOTHING;
			`, chunkID, collectionID)
			if err != nil {
				return errors.WithStack(err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

var _ backup.Snapshotable = &Index{}
