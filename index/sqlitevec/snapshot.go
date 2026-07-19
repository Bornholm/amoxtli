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
}

// GenerateSnapshot implements snapshot.Snapshotable.
func (i *Index) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	r, w := io.Pipe()

	go func() {
		defer w.Close()

		encoder := gob.NewEncoder(w)

		metadata := SnapshottedMetadata{
			Model: i.model,
		}

		if err := encoder.Encode(metadata); err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}

		err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
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
					COALESCE(json_group_array(ec.collection_id) FILTER ( WHERE ec.collection_id IS NOT NULL ), '[]') AS collections
				FROM embeddings e
				LEFT JOIN embeddings_collections ec ON e.id = ec.embeddings_id
				GROUP BY e.id, e.source, e.section_id, e.chunk_index
				;
			`

			stmt, _, err := conn.Prepare(sql)
			if err != nil {
				return errors.WithStack(err)
			}

			defer stmt.Close()

			for stmt.Step() {
				record := SnapshottedRecord{}
				record.Source = stmt.ColumnText(1)
				record.SectionID = stmt.ColumnText(2)
				record.ChunkIndex = stmt.ColumnInt(3)
				record.Embeddings = stmt.ColumnBlob(4, []byte{})
				rawCollections := stmt.ColumnBlob(5, []byte{})
				if err := json.Unmarshal(rawCollections, &record.Collections); err != nil {
					return errors.WithStack(err)
				}

				if err := encoder.Encode(record); err != nil {
					return errors.WithStack(err)
				}
			}

			if err := stmt.Err(); err != nil {
				return errors.WithStack(err)
			}

			return nil
		}, sqlite3.LOCKED, sqlite3.BUSY)
		if err != nil {
			w.CloseWithError(errors.WithStack(err))
			return
		}
	}()

	return io.NopCloser(r), nil
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

	err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		for _, sql := range []string{
			"DELETE FROM embeddings_vec2;",
			"DELETE FROM embeddings_vec_map;",
			"DELETE FROM embeddings_collections;",
			"DELETE FROM embeddings;",
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

	err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
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

		restoreRecord := func(record *SnapshottedRecord) error {
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
