package pipeline

import (
	"context"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// All implements index.Index.
func (i *Index) All(ctx context.Context, yield func(model.SectionID) bool) error {
	for identified := range i.indexes {
		if err := identified.Index().All(ctx, yield); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}
