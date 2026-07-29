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

// CollectionLanguageLister reports the natural languages a corpus is written
// in, as ISO 639-1 codes ordered by decreasing number of documents. It is used
// by the query translation transformer to know which languages are worth
// translating a query into. A nil or empty ids selects every collection.
//
// Only documents carrying a detected language are counted, so an entirely
// undetectable corpus yields an empty slice — and translation is skipped rather
// than guessed at.
type CollectionLanguageLister interface {
	ListCollectionLanguages(ctx context.Context, ids []model.CollectionID) ([]string, error)
}
