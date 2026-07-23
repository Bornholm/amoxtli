package bleve

import (
	"context"

	"github.com/bornholm/amoxtli/backup"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// RestoreDocuments implements service.Restorable.
func (i *Index) RestoreDocuments(ctx context.Context, documents []model.Document) error {
	batch := i.index.NewBatch()
	flush := func() error {
		if batch.Size() == 0 {
			return nil
		}

		if err := i.index.Batch(batch); err != nil {
			return errors.WithStack(err)
		}

		batch.Reset()

		return nil
	}

	for _, d := range documents {
		err := model.WalkSections(d, func(s model.Section) error {
			id, resource, err := i.getIndexableResource(ctx, s)
			if err != nil {
				return errors.WithStack(err)
			}

			if resource == nil {
				return nil
			}

			if err := batch.Index(id, resource); err != nil {
				return errors.WithStack(err)
			}

			// Cap the in-flight batch: a restore can carry far more sections
			// than a single document, and holding them all in memory before
			// the first apply is unnecessary.
			if batch.Size() >= batchFlushSize {
				return flush()
			}

			return nil
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return flush()
}

var _ backup.Restorable = &Index{}
