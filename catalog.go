package amoxtli

import (
	"context"
	"net/url"

	"github.com/bornholm/amoxtli/blob"
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
	// Filter keeps the documents whose metadata it matches — a facet value
	// picked by a user, typically. Like facets, it is evaluated over the
	// document headers: the whole collection is read to count the total.
	Filter index.Filter
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

	if len(opts.Filter) > 0 {
		return c.listFilteredDocuments(ctx, collectionID, query, opts.Filter, page, limit)
	}

	documents, total, err := c.store.QueryDocumentsByCollectionID(ctx, collectionID, query)
	if err != nil {
		return nil, 0, errors.WithStack(err)
	}

	return documents, total, nil
}

// listFilteredDocuments pages through the documents of the collection
// matching the metadata filter.
func (c *Codex) listFilteredDocuments(ctx context.Context, collectionID model.CollectionID, query ingest.QueryDocumentsOptions, filter index.Filter, page, limit int) ([]model.PersistedDocument, int64, error) {
	if err := filter.Validate(); err != nil {
		return nil, 0, errors.WithStack(err)
	}

	var (
		matching []model.PersistedDocument
		total    int64
		skip     = page * limit
	)

	pageSize := facetPageSize
	query.Limit = &pageSize

	for storePage := 0; ; storePage++ {
		if err := ctx.Err(); err != nil {
			return nil, 0, errors.WithStack(err)
		}

		query.Page = &storePage
		documents, _, err := c.store.QueryDocumentsByCollectionID(ctx, collectionID, query)
		if err != nil {
			return nil, 0, errors.WithStack(err)
		}

		for _, d := range documents {
			if !filter.Matches(model.Metadata(d)) {
				continue
			}
			if total >= int64(skip) && len(matching) < limit {
				matching = append(matching, d)
			}
			total++
		}

		if len(documents) < pageSize {
			return matching, total, nil
		}
	}
}

// CountDocuments returns the number of documents of the codex, each counted
// once whatever the number of collections holding it.
func (c *Codex) CountDocuments(ctx context.Context) (int64, error) {
	page, limit := 0, 1
	_, total, err := c.store.QueryDocuments(ctx, ingest.QueryDocumentsOptions{Page: &page, Limit: &limit, HeaderOnly: true})
	if err != nil {
		return 0, errors.WithStack(err)
	}
	return total, nil
}

// GetDocument returns a document with its sections and collections.
func (c *Codex) GetDocument(ctx context.Context, id model.DocumentID) (model.PersistedDocument, error) {
	document, err := c.store.GetDocumentByID(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return document, nil
}

// GetDocumentBySource returns the document indexed from a source, with its
// sections and collections, or ErrNotFound.
func (c *Codex) GetDocumentBySource(ctx context.Context, source *url.URL) (model.PersistedDocument, error) {
	limit := 1
	documents, _, err := c.store.QueryDocuments(ctx, ingest.QueryDocumentsOptions{MatchingSource: source, Limit: &limit, HeaderOnly: true})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(documents) == 0 {
		return nil, errors.WithStack(ErrNotFound)
	}

	document, err := c.store.GetDocumentByID(ctx, documents[0].ID())
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

// DocumentsReferencingBlob returns the documents whose content references the
// blob — to allow an image only to the users who may read one of them.
func (c *Codex) DocumentsReferencingBlob(ctx context.Context, hash blob.Hash) ([]model.DocumentID, error) {
	lister, ok := c.store.(ingest.BlobReferrerLister)
	if !ok {
		return nil, errors.WithStack(ErrUnsupported)
	}

	ids, err := lister.DocumentsReferencingBlob(ctx, string(hash))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return ids, nil
}
