package sqlitevec

import (
	"context"

	"github.com/bornholm/amoxtli/model"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// All implements index.Index.
func (i *Index) All(ctx context.Context, yield func(model.SectionID) bool) error {
	batchSize := 1000
	// The pages follow the id order, from the last id read: a write between
	// two pages neither skips nor repeats the rows that were already there.
	var after int64

	for {
		var batch []model.SectionID
		from := after
		err := i.withLockedRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
			// A retry reads the whole page again, from the same id.
			batch, after = batch[:0], from
			sql := `SELECT id, section_id FROM embeddings WHERE id > ? ORDER BY id LIMIT ?;`

			stmt, _, err := conn.Prepare(sql)
			if err != nil {
				return errors.WithStack(err)
			}

			defer stmt.Close()

			if err := stmt.BindInt64(1, from); err != nil {
				return errors.WithStack(err)
			}

			if err := stmt.BindInt(2, batchSize); err != nil {
				return errors.WithStack(err)
			}

			for stmt.Step() {
				after = stmt.ColumnInt64(0)
				batch = append(batch, model.SectionID(stmt.ColumnText(1)))
			}

			if err := stmt.Err(); err != nil {
				return errors.WithStack(err)
			}

			return nil
		}, sqlite3.LOCKED, sqlite3.BUSY)
		if err != nil {
			return errors.WithStack(err)
		}

		if len(batch) == 0 {
			return nil
		}

		for _, id := range batch {
			if !yield(id) {
				return nil
			}
		}

		batch = nil

	}
}
