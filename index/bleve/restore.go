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

			return nil
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}

	if err := i.index.Batch(batch); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

var _ backup.Restorable = &Index{}
