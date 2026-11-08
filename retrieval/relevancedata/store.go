package relevancedata

import (
	"context"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// WalkStore returns a walk over every section of every document of the store,
// for Sample.
func WalkStore(ctx context.Context, store ingest.Store) func(func(model.Section) error) error {
	return func(fn func(model.Section) error) error {
		limit := 100
		for page := 0; ; page++ {
			documents, _, err := store.QueryDocuments(ctx, ingest.QueryDocumentsOptions{Limit: &limit, Page: &page})
			if err != nil {
				return errors.WithStack(err)
			}
			for _, d := range documents {
				if err := model.WalkSections(d, fn); err != nil {
					return errors.WithStack(err)
				}
			}
			if len(documents) < limit {
				return nil
			}
		}
	}
}
