package amoxtli

import (
	"context"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// ListCollections returns every collection, headers only.
func (c *Codex) ListCollections(ctx context.Context) ([]model.PersistedCollection, error) {
	collections, err := c.store.QueryCollections(ctx, ingest.QueryCollectionsOptions{HeaderOnly: true})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return collections, nil
}

// GetCollection returns a collection by its ID.
func (c *Codex) GetCollection(ctx context.Context, id model.CollectionID) (model.PersistedCollection, error) {
	collection, err := c.store.GetCollectionByID(ctx, id, false)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return collection, nil
}

// CollectionUpdates holds the fields of a collection to change; nil fields are
// left untouched.
type CollectionUpdates = ingest.CollectionUpdates

// UpdateCollection changes the label and/or description of a collection.
func (c *Codex) UpdateCollection(ctx context.Context, id model.CollectionID, updates CollectionUpdates) (model.PersistedCollection, error) {
	collection, err := c.store.UpdateCollection(ctx, id, updates)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return collection, nil
}

// CollectionStats returns the number of documents and the languages of a
// collection.
func (c *Codex) CollectionStats(ctx context.Context, id model.CollectionID) (*model.CollectionStats, error) {
	stats, err := c.store.GetCollectionStats(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return stats, nil
}

// DeleteCollection deletes a collection. Its documents are not deleted: those
// left without any collection become orphans, which CleanupIndex removes
// together with their index entries.
func (c *Codex) DeleteCollection(ctx context.Context, id model.CollectionID) error {
	if err := c.store.DeleteCollection(ctx, id); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// ListDocumentsOptions pages through the documents of a collection.
type ListDocumentsOptions struct {
	// Page is the 0-based page index.
	Page int
	// Limit is the page size; 0 defaults to 50.
	Limit int
	// SourcePattern keeps the documents whose source contains it.
	SourcePattern string
}

// ListDocuments returns a page of the documents of a collection, headers only,
// with the total number of matching documents.
func (c *Codex) ListDocuments(ctx context.Context, collectionID model.CollectionID, opts ListDocumentsOptions) ([]model.PersistedDocument, int64, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	page := max(opts.Page, 0)

	query := ingest.QueryDocumentsOptions{
		Page:       &page,
		Limit:      &limit,
		HeaderOnly: true,
	}
	if opts.SourcePattern != "" {
		query.SourcePattern = &opts.SourcePattern
	}

	documents, total, err := c.store.QueryDocumentsByCollectionID(ctx, collectionID, query)
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}

	return documents, total, nil
}

// GetDocument returns a document with its sections and collections.
func (c *Codex) GetDocument(ctx context.Context, id model.DocumentID) (model.PersistedDocument, error) {
	document, err := c.store.GetDocumentByID(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return document, nil
}

// DeleteDocument removes a document and its index entries.
func (c *Codex) DeleteDocument(ctx context.Context, id model.DocumentID) error {
	document, err := c.store.GetDocumentByID(ctx, id)
	if err != nil {
		return errors.WithStack(err)
	}

	if source := document.Source(); source != nil {
		if err := c.index.DeleteBySource(ctx, source); err != nil {
			return errors.WithStack(err)
		}
	}

	if err := c.store.DeleteDocumentByID(ctx, id); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// UpdateDocumentMetadata replaces the metadata of a document, in the store and
// in the indexes keeping a copy of it, without reindexing its content.
func (c *Codex) UpdateDocumentMetadata(ctx context.Context, id model.DocumentID, metadata map[string]any) error {
	updater, ok := c.store.(ingest.DocumentUpdater)
	if !ok {
		return errors.WithStack(ErrUnsupported)
	}

	document, err := c.store.GetDocumentByID(ctx, id)
	if err != nil {
		return errors.WithStack(err)
	}

	if err := updater.UpdateDocumentMetadata(ctx, id, metadata); err != nil {
		return errors.WithStack(err)
	}

	if source := document.Source(); source != nil {
		if err := index.UpdateMetadata(ctx, c.index, source, metadata); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// SetDocumentCollections replaces the collections a document belongs to, then
// reindexes it: indexes partition their entries by collection. Its content is
// not converted again, and embeddings come from the cache when one is
// configured.
func (c *Codex) SetDocumentCollections(ctx context.Context, id model.DocumentID, collections ...model.CollectionID) error {
	updater, ok := c.store.(ingest.DocumentUpdater)
	if !ok {
		return errors.WithStack(ErrUnsupported)
	}

	if err := updater.SetDocumentCollections(ctx, id, collections); err != nil {
		return errors.WithStack(err)
	}

	document, err := c.store.GetDocumentByID(ctx, id)
	if err != nil {
		return errors.WithStack(err)
	}

	if source := document.Source(); source != nil {
		if err := c.index.DeleteBySource(ctx, source); err != nil {
			return errors.WithStack(err)
		}
	}

	if err := c.index.Index(ctx, document); err != nil {
		return errors.WithStack(err)
	}

	return nil
}
