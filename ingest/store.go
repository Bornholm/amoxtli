package ingest

import (
	"context"
	"net/url"

	"github.com/bornholm/amoxtli/model"
)

// DocumentDigest holds a minimal projection of a document used for bulk change detection.
type DocumentDigest struct {
	ID     model.DocumentID
	Source string
	ETag   string
}

// Store persists documents, sections and collections backing the ingestion pipeline.
type Store interface {
	// ListDocumentDigests returns (Source, ETag) pairs for documents whose source URL
	// starts with sourcePrefix. Results are paginated; pageSize=0 defaults to 500.
	ListDocumentDigests(ctx context.Context, sourcePrefix string, page int, pageSize int) ([]DocumentDigest, error)

	GetDocumentByID(ctx context.Context, id model.DocumentID) (model.PersistedDocument, error)
	SaveDocuments(ctx context.Context, documents ...model.Document) error
	DeleteDocumentBySource(ctx context.Context, source *url.URL) error
	DeleteDocumentByID(ctx context.Context, ids ...model.DocumentID) error
	QueryDocuments(ctx context.Context, opts QueryDocumentsOptions) ([]model.PersistedDocument, int64, error)

	// QueryDocumentsByCollectionID retrieves all documents belonging to a specific collection.
	QueryDocumentsByCollectionID(ctx context.Context, collectionID model.CollectionID, opts QueryDocumentsOptions) ([]model.PersistedDocument, int64, error)

	GetSectionByID(ctx context.Context, id model.SectionID) (model.Section, error)
	GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error)
	SectionExists(ctx context.Context, id model.SectionID) (bool, error)
	// SectionsExist checks the existence of multiple sections in a single query.
	SectionsExist(ctx context.Context, ids []model.SectionID) (map[model.SectionID]bool, error)

	GetCollectionByID(ctx context.Context, id model.CollectionID, full bool) (model.PersistedCollection, error)
	QueryCollections(ctx context.Context, opts QueryCollectionsOptions) ([]model.PersistedCollection, error)
	CreateCollection(ctx context.Context, label string) (model.PersistedCollection, error)
	UpdateCollection(ctx context.Context, id model.CollectionID, updates CollectionUpdates) (model.PersistedCollection, error)
	GetCollectionStats(ctx context.Context, id model.CollectionID) (*model.CollectionStats, error)
	DeleteCollection(ctx context.Context, id model.CollectionID) error
}

type QueryDocumentsOptions struct {
	Page  *int
	Limit *int

	// Do not retrieve associations
	HeaderOnly bool

	// Filters

	// Documents matching the given source
	MatchingSource *url.URL

	// Documents without parent collection
	Orphaned *bool

	// Documents matching the given source pattern (LIKE %pattern%)
	SourcePattern *string

	// Column to sort by: "source" or "created_at" (default)
	SortBy *string

	// Sort direction: "asc" or "desc" (default)
	SortOrder *string
}

type QueryCollectionsOptions struct {
	Page  *int
	Limit *int

	// Do not retrieve associations
	HeaderOnly bool

	// Filters

	// Collections with these ids
	IDs []model.CollectionID
}

type CollectionUpdates struct {
	Label       *string
	Description *string
}
