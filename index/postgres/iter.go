package postgres

import (
	"context"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// All implements index.Index.
func (i *Index) All(ctx context.Context, yield func(model.SectionID) bool) error {
	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	batchSize := 1000
	page := 0

	for {
		var batch []model.SectionID

		rows, err := pool.Query(ctx, `SELECT section_id FROM amoxtli_chunks ORDER BY id LIMIT $1 OFFSET $2`, batchSize, page*batchSize)
		if err != nil {
			return errors.WithStack(err)
		}

		for rows.Next() {
			var sectionID string
			if err := rows.Scan(&sectionID); err != nil {
				rows.Close()
				return errors.WithStack(err)
			}
			batch = append(batch, model.SectionID(sectionID))
		}

		rows.Close()

		if err := rows.Err(); err != nil {
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

		page++
	}
}
