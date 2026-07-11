package bleve

import (
	"context"

	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// All implements index.Index.
func (i *Index) All(ctx context.Context, yield func(model.SectionID) bool) error {
	advanced, err := i.index.Advanced()
	if err != nil {
		return errors.WithStack(err)
	}

	reader, err := advanced.Reader()
	if err != nil {
		return errors.WithStack(err)
	}

	defer reader.Close()

	ids, err := reader.DocIDReaderAll()
	if err != nil {
		return errors.WithStack(err)
	}

	defer ids.Close()

	for {
		internalID, err := ids.Next()
		if err != nil {
			return errors.WithStack(err)
		}

		if internalID == nil {
			return nil
		}

		id, err := reader.ExternalID(internalID)
		if err != nil {
			return errors.WithStack(err)
		}

		if !yield(model.SectionID(id)) {
			return nil
		}
	}
}
