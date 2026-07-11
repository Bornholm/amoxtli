package pipeline

import (
	"context"

	"github.com/bornholm/amoxtli/model"
)

// SectionStore provides access to the content of indexed sections.
// It is satisfied structurally by ingest.Store.
type SectionStore interface {
	GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error)
}

// CollectionLister provides the labels and descriptions of collections,
// used by the HyDE query transformer to orient the hypothetical answer.
type CollectionLister interface {
	ListCollections(ctx context.Context, ids []model.CollectionID) ([]model.Collection, error)
}
