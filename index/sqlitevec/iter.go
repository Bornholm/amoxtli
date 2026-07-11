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
	page := 0

	for {
		var batch []model.SectionID
		err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
			sql := `SELECT section_id FROM embeddings LIMIT ? OFFSET ?;`

			stmt, _, err := conn.Prepare(sql)
			if err != nil {
				return errors.WithStack(err)
			}

			defer stmt.Close()

			if err := stmt.BindInt(1, batchSize); err != nil {
				return errors.WithStack(err)
			}

			if err := stmt.BindInt(2, page*batchSize); err != nil {
				return errors.WithStack(err)
			}

			for stmt.Step() {
				sectionID := model.SectionID(stmt.ColumnText(0))
				batch = append(batch, sectionID)
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

		page++
	}
}
