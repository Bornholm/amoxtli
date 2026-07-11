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
			sql := `
				SELECT
					e.id,
					e.source,
					e.section_id,
					e.embeddings,
					COALESCE(json_group_array(ec.collection_id), '[]') AS collections
				FROM embeddings e
				LEFT JOIN embeddings_collections ec ON e.id = ec.embeddings_id
				GROUP BY e.id, e.source, e.section_id
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
				record.Embeddings = stmt.ColumnBlob(3, []byte{})
				rawCollections := stmt.ColumnBlob(4, []byte{})
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
		if err := conn.Exec("DELETE FROM embeddings;"); err != nil {
			return errors.WithStack(err)
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

					batch = nil
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
		slog.DebugContext(ctx, "restored record batch", slog.Any("batchSize", len(records)), slog.Duration("duration", time.Now().Sub(start)))
	}()

	err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {

		restoreRecord := func(record *SnapshottedRecord) error {
			deleteStmt, _, err := conn.Prepare("DELETE FROM embeddings WHERE source = ? and section_id = ?;")
			if err != nil {
				return errors.WithStack(err)
			}

			defer deleteStmt.Close()

			if err := deleteStmt.BindText(1, record.Source); err != nil {
				return errors.WithStack(err)
			}

			if err := deleteStmt.BindText(2, record.SectionID); err != nil {
				return errors.WithStack(err)
			}

			if err := deleteStmt.Exec(); err != nil {
				return errors.WithStack(err)
			}

			if err := deleteStmt.Close(); err != nil {
				return errors.WithStack(err)
			}

			insertStmt, _, err := conn.Prepare("INSERT INTO embeddings ( source, section_id, embeddings ) VALUES (?, ?, ?) RETURNING id;")
			if err != nil {
				return errors.WithStack(err)
			}

			defer insertStmt.Close()

			if err := insertStmt.BindText(1, record.Source); err != nil {
				return errors.WithStack(err)
			}

			if err := insertStmt.BindText(2, record.SectionID); err != nil {
				return errors.WithStack(err)
			}

			if err := insertStmt.BindBlob(3, record.Embeddings); err != nil {
				return errors.WithStack(err)
			}

			insertStmt.Step()

			if err := insertStmt.Err(); err != nil {
				return errors.WithStack(err)
			}

			embeddingsID := insertStmt.ColumnInt(0)

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
