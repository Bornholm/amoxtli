package sqlitevec

import (
	"context"
	"encoding/gob"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/bornholm/amoxtli/backup"
	"github.com/bornholm/amoxtli/model"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

func init() {
	gob.Register(SnapshottedRecord{})
	gob.Register(SnapshottedMetadata{})
}

type SnapshottedMetadata struct {
	Model string
}

type SnapshottedRecord struct {
	Source      string
	SectionID   string
	ChunkIndex  int
	Embeddings  []byte
	Collections []string
	// Metadata is the document's metadata JSON (schema v3), repeated on each of
	// its chunks. Snapshots taken before v3 decode it as "", which restores a
	// document without metadata — the same state an index had back then.
	Metadata string
}

// snapshotBatch is how many records GenerateSnapshot reads under the
// connection lock at a time.
const snapshotBatch = 500

// GenerateSnapshot implements snapshot.Snapshotable.
//
// The records are read in batches under the connection lock, and written to
// the stream with the lock released: a slow or departed reader never holds
// up the index. Each batch is read in one statement; a write between two
// batches may or may not be in the snapshot, as a write during a file copy
// would. Closing the returned reader, or ctx ending, stops the export. Do not
// restore an index from its own export: the restore empties the index before
// the export has read it.
func (i *Index) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	r, w := io.Pipe()

	go func() {
		defer w.Close()
		stop := context.AfterFunc(ctx, func() { w.CloseWithError(ctx.Err()) })
		defer stop()

		encoder := gob.NewEncoder(w)

		metadata := SnapshottedMetadata{
			Model: i.model,
		}

		if err := encoder.Encode(metadata); err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}

		var after int64
		for {
			batch, last, err := i.snapshotRecords(ctx, after)
			if err != nil {
				w.CloseWithError(errors.WithStack(err))
				return
			}
			for _, record := range batch {
				if err := encoder.Encode(record); err != nil {
					w.CloseWithError(errors.WithStack(err))
					return
				}
			}
			if len(batch) < snapshotBatch {
				return
			}
			after = last
		}
	}()

	return r, nil
}

// snapshotRecords reads, under the connection lock, the records of the
// chunks whose id follows after, in id order, and the last id read.
func (i *Index) snapshotRecords(ctx context.Context, after int64) ([]SnapshottedRecord, int64, error) {
	var (
		batch []SnapshottedRecord
		last  int64
	)
	err := i.withLockedRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		// A retry reads the whole batch again.
		batch, last = batch[:0], after

		// One record per chunk. The vector blob lives in vec0; a chunk in
		// several collections has one identical vector row per collection,
		// so any of them (MIN map id) is representative.
		sql := `
			SELECT
				e.id,
				e.source,
				e.section_id,
				e.chunk_index,
				(
					SELECT v.embedding FROM embeddings_vec2 v
					WHERE v.rowid = ( SELECT MIN(m.id) FROM embeddings_vec_map m WHERE m.embeddings_id = e.id )
				),
				COALESCE(json_group_array(ec.collection_id) FILTER ( WHERE ec.collection_id IS NOT NULL ), '[]') AS collections,
				COALESCE(dm.metadata, '') AS metadata
			FROM embeddings e
			LEFT JOIN embeddings_collections ec ON e.id = ec.embeddings_id
			LEFT JOIN document_metadata dm ON dm.source = e.source
			WHERE e.id > ?
			GROUP BY e.id, e.source, e.section_id, e.chunk_index, dm.metadata
			ORDER BY e.id
			LIMIT ?
			;
		`

		stmt, _, err := conn.Prepare(sql)
		if err != nil {
			return errors.WithStack(err)
		}

		defer stmt.Close()

		if err := stmt.BindInt64(1, after); err != nil {
			return errors.WithStack(err)
		}
		if err := stmt.BindInt(2, snapshotBatch); err != nil {
			return errors.WithStack(err)
		}

		for stmt.Step() {
			record := SnapshottedRecord{}
			last = stmt.ColumnInt64(0)
			record.Source = stmt.ColumnText(1)
			record.SectionID = stmt.ColumnText(2)
			record.ChunkIndex = stmt.ColumnInt(3)
			record.Embeddings = stmt.ColumnBlob(4, []byte{})
			rawCollections := stmt.ColumnBlob(5, []byte{})
			if err := json.Unmarshal(rawCollections, &record.Collections); err != nil {
				return errors.WithStack(err)
			}
			record.Metadata = stmt.ColumnText(6)
			batch = append(batch, record)
		}

		return errors.WithStack(stmt.Err())
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, 0, err
	}
	return batch, last, nil
}

// RestoreSnapshot implements snapshot.Snapshotable.
func (i *Index) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	decoder := gob.NewDecoder(r)

	metadata := SnapshottedMetadata{}

	if err := decoder.Decode(&metadata); err != nil {
		return errors.WithStack(err)
	}

	if metadata.Model != i.model {
		return errors.Errorf("could not restore snapshot with a different embedding model '%s'", metadata.Model)
	}

	err := i.withLockedRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		for _, sql := range []string{
			"DELETE FROM embeddings_vec2;",
			"DELETE FROM embeddings_vec_map;",
			"DELETE FROM embeddings_collections;",
			"DELETE FROM embeddings;",
			"DELETE FROM document_metadata;",
		} {
			if err := conn.Exec(sql); err != nil {
				return errors.WithStack(err)
			}
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
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

			batch = nil
		}

	}
}

func (i *Index) restoreRecords(ctx context.Context, records ...*SnapshottedRecord) error {
	start := time.Now()
	defer func() {
		slog.DebugContext(ctx, "restored record batch", slog.Any("batchSize", len(records)), slog.Duration("duration", time.Since(start)))
	}()

	err := i.withLockedRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		insertStmt, _, err := conn.Prepare("INSERT INTO embeddings ( source, section_id, chunk_index ) VALUES (?, ?, ?) RETURNING id;")
		if err != nil {
			return errors.WithStack(err)
		}
		defer insertStmt.Close()

		mapStmt, _, err := conn.Prepare("INSERT INTO embeddings_vec_map ( embeddings_id, collection_id ) VALUES (?, ?) RETURNING id;")
		if err != nil {
			return errors.WithStack(err)
		}
		defer mapStmt.Close()

		// The snapshotted vectors are already normalized/sliced, so they are
		// reinserted as-is (vec_slice would reject a shorter stored vector).
		vecSQL := "INSERT INTO embeddings_vec2 ( rowid, embedding, collection_id ) VALUES (?, ?, ?);"
		if i.hasCoarse() {
			vecSQL = "INSERT INTO embeddings_vec2 ( rowid, embedding, embedding_coarse, collection_id ) VALUES (?, ?, vec_quantize_binary(?), ?);"
		}
		vecStmt, _, err := conn.Prepare(vecSQL)
		if err != nil {
			return errors.WithStack(err)
		}
		defer vecStmt.Close()

		// The metadata is repeated on every chunk of a document, so the insert
		// is an upsert: the first chunk writes it, the others rewrite the same
		// value.
		metadataStmt, _, err := conn.Prepare(`
			INSERT INTO document_metadata ( source, metadata ) VALUES (?, ?)
			ON CONFLICT (source) DO UPDATE SET metadata = excluded.metadata;
		`)
		if err != nil {
			return errors.WithStack(err)
		}
		defer metadataStmt.Close()

		restoreRecord := func(record *SnapshottedRecord) error {
			if record.Metadata != "" {
				if err := metadataStmt.BindText(1, record.Source); err != nil {
					return errors.WithStack(err)
				}
				if err := metadataStmt.BindText(2, record.Metadata); err != nil {
					return errors.WithStack(err)
				}
				if err := metadataStmt.Exec(); err != nil {
					return errors.WithStack(err)
				}
				if err := metadataStmt.Reset(); err != nil {
					return errors.WithStack(err)
				}
			}

			if err := insertStmt.BindText(1, record.Source); err != nil {
				return errors.WithStack(err)
			}
			if err := insertStmt.BindText(2, record.SectionID); err != nil {
				return errors.WithStack(err)
			}
			if err := insertStmt.BindInt(3, record.ChunkIndex); err != nil {
				return errors.WithStack(err)
			}

			if hasRow := insertStmt.Step(); !hasRow {
				if err := insertStmt.Err(); err != nil {
					return errors.WithStack(err)
				}
				return errors.New("no id returned")
			}

			embeddingsID := insertStmt.ColumnInt(0)
			if err := insertStmt.Reset(); err != nil {
				return errors.WithStack(err)
			}

			partitions := []string{""}
			if len(record.Collections) > 0 {
				partitions = record.Collections
			}

			for _, partition := range partitions {
				if err := mapStmt.BindInt(1, embeddingsID); err != nil {
					return errors.WithStack(err)
				}
				if err := mapStmt.BindText(2, partition); err != nil {
					return errors.WithStack(err)
				}
				if hasRow := mapStmt.Step(); !hasRow {
					if err := mapStmt.Err(); err != nil {
						return errors.WithStack(err)
					}
					return errors.New("no vec map id returned")
				}
				vecRowID := mapStmt.ColumnInt64(0)
				if err := mapStmt.Reset(); err != nil {
					return errors.WithStack(err)
				}

				if err := vecStmt.BindInt64(1, vecRowID); err != nil {
					return errors.WithStack(err)
				}
				if err := vecStmt.BindBlob(2, record.Embeddings); err != nil {
					return errors.WithStack(err)
				}
				next := 3
				if i.hasCoarse() {
					if err := vecStmt.BindBlob(3, record.Embeddings); err != nil {
						return errors.WithStack(err)
					}
					next = 4
				}
				if err := vecStmt.BindText(next, partition); err != nil {
					return errors.WithStack(err)
				}
				if err := vecStmt.Exec(); err != nil {
					return errors.WithStack(err)
				}
				if err := vecStmt.Reset(); err != nil {
					return errors.WithStack(err)
				}
			}

			for _, collectionID := range record.Collections {
				if err := i.insertCollection(ctx, conn, embeddingsID, model.CollectionID(collectionID)); err != nil {
					return errors.WithStack(err)
				}
			}

			return nil
		}

		for _, r := range records {
			if err := restoreRecord(r); err != nil {
				return errors.WithStack(err)
			}
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

var _ backup.Snapshotable = &Index{}
