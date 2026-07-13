package gorm

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/url"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GetDocumentsMetadataBySources implements ingest.MetadataProvider.
func (s *Store) GetDocumentsMetadataBySources(ctx context.Context, sources []string) (map[string]map[string]any, error) {
	result := make(map[string]map[string]any, len(sources))
	if len(sources) == 0 {
		return result, nil
	}

	type row struct {
		Source   string
		Metadata []byte
	}

	var rows []row

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		return db.Model(&Document{}).Select("source, metadata").Where("source IN ?", sources).Scan(&rows).Error
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for _, r := range rows {
		if len(r.Metadata) == 0 {
			result[r.Source] = nil
			continue
		}
		var metadata map[string]any
		if err := json.Unmarshal(r.Metadata, &metadata); err != nil {
			slog.WarnContext(ctx, "could not unmarshal document metadata", slog.String("source", r.Source), slog.Any("error", errors.WithStack(err)))
			result[r.Source] = nil
			continue
		}
		result[r.Source] = metadata
	}

	return result, nil
}

// ListDocumentDigests implements ingest.Store.
func (s *Store) ListDocumentDigests(ctx context.Context, sourcePrefix string, page int, pageSize int) ([]ingest.DocumentDigest, error) {
	if pageSize <= 0 {
		pageSize = 500
	}

	type row struct {
		ID     string
		Source string
		ETag   string
	}

	var rows []row

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		q := db.Model(&Document{}).Select("id, source, etag")
		if sourcePrefix != "" {
			q = q.Where("source LIKE ?", sourcePrefix+"%")
		}
		return q.Offset(page * pageSize).Limit(pageSize).Scan(&rows).Error
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	digests := make([]ingest.DocumentDigest, len(rows))
	for i, r := range rows {
		digests[i] = ingest.DocumentDigest{
			ID:     model.DocumentID(r.ID),
			Source: r.Source,
			ETag:   r.ETag,
		}
	}

	return digests, nil
}

// DeleteDocumentByID implements ingest.Store.
func (s *Store) DeleteDocumentByID(ctx context.Context, ids ...model.DocumentID) error {
	err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
		if err := db.Model(&Section{}).Delete("document_id in ?", ids).Error; err != nil {
			return errors.WithStack(err)
		}

		if err := db.Model(&Document{}).Delete("id in ?", ids).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// SectionsExist implements ingest.Store.
func (s *Store) SectionsExist(ctx context.Context, ids []model.SectionID) (map[model.SectionID]bool, error) {
	result := make(map[model.SectionID]bool, len(ids))
	if len(ids) == 0 {
		return result, nil
	}

	strIDs := make([]string, len(ids))
	for i, id := range ids {
		strIDs[i] = string(id)
		result[id] = false
	}

	type sectionIDRow struct {
		ID string `gorm:"column:id"`
	}

	var rows []sectionIDRow

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		return db.Model(&Section{}).Select("id").Where("id IN ?", strIDs).Scan(&rows).Error
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	for _, row := range rows {
		result[model.SectionID(row.ID)] = true
	}

	return result, nil
}

// SectionExists implements ingest.Store.
func (s *Store) SectionExists(ctx context.Context, id model.SectionID) (bool, error) {
	var exists bool

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		var count int64
		if err := db.Model(&Section{}).Where("id = ?", string(id)).Count(&count).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(ingest.ErrNotFound)
			}

			return errors.WithStack(err)
		}

		exists = count > 0

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return false, errors.WithStack(err)
	}

	return exists, nil
}

// GetDocumentByID implements ingest.Store.
func (s *Store) GetDocumentByID(ctx context.Context, id model.DocumentID) (model.PersistedDocument, error) {
	var document Document

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		if err := db.Preload(clause.Associations).Preload("Sections", "parent_id is null").First(&document, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(ingest.ErrNotFound)
			}

			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &wrappedDocument{&document}, nil
}

// GetCollectionByID implements ingest.Store.
func (s *Store) GetCollectionByID(ctx context.Context, id model.CollectionID, full bool) (model.PersistedCollection, error) {
	var collection Collection

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		query := db
		if full {
			query = query.Preload(clause.Associations)
		}
		if err := query.First(&collection, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(ingest.ErrNotFound)
			}

			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &wrappedCollection{&collection}, nil
}

// GetCollectionStats implements ingest.Store.
func (s *Store) GetCollectionStats(ctx context.Context, id model.CollectionID) (*model.CollectionStats, error) {
	db, err := s.getDatabase(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	stats := &model.CollectionStats{
		TotalDocuments: db.Model(&Collection{ID: string(id)}).Association("Documents").Count(),
	}

	return stats, nil
}

// CreateCollection implements ingest.Store.
func (s *Store) CreateCollection(ctx context.Context, label string) (model.PersistedCollection, error) {
	var collection model.PersistedCollection
	err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
		coll := Collection{
			ID:    string(model.NewCollectionID()),
			Label: label,
		}

		if err := db.Create(&coll).Error; err != nil {
			return errors.WithStack(err)
		}

		collection = &wrappedCollection{&coll}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return collection, nil
}

// UpdateCollection implements ingest.Store.
func (s *Store) UpdateCollection(ctx context.Context, id model.CollectionID, updates ingest.CollectionUpdates) (model.PersistedCollection, error) {
	var collection Collection

	err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
		// First, find the existing collection
		if err := db.First(&collection, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(ingest.ErrNotFound)
			}
			return errors.WithStack(err)
		}

		// Prepare updates map
		updateFields := make(map[string]interface{})

		if updates.Label != nil {
			updateFields["label"] = *updates.Label
		}

		if updates.Description != nil {
			updateFields["description"] = *updates.Description
		}

		// Only perform update if there are fields to update
		if len(updateFields) > 0 {
			if err := db.Model(&collection).Updates(updateFields).Error; err != nil {
				return errors.WithStack(err)
			}
		}

		// Reload the collection to get the updated values
		if err := db.Preload(clause.Associations).First(&collection, "id = ?", id).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &wrappedCollection{&collection}, nil
}

// DeleteCollection implements ingest.Store.
func (s *Store) DeleteCollection(ctx context.Context, id model.CollectionID) error {
	err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
		if err := db.Model(&Collection{}).Delete("id = ?", id).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// QueryCollections implements ingest.Store.
func (s *Store) QueryCollections(ctx context.Context, opts ingest.QueryCollectionsOptions) ([]model.PersistedCollection, error) {
	db, err := s.getDatabase(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	query := db.Model(&Collection{})

	if len(opts.IDs) > 0 {
		rawCollectionIDs := slices.Collect(func(yield func(string) bool) {
			for _, id := range opts.IDs {
				if !yield(string(id)) {
					return
				}
			}
		})
		query = query.Where("id in ?", rawCollectionIDs)
	}

	if opts.Page != nil {
		limit := 100
		if opts.Limit != nil {
			limit = *opts.Limit
		}

		query = query.Offset(*opts.Page * limit)
	}

	if opts.Limit != nil {
		query = query.Limit(*opts.Limit)
	}

	if !opts.HeaderOnly {
		query = query.Preload(clause.Associations)
	}

	var collections []*Collection

	if err := query.Find(&collections).Error; err != nil {
		return nil, errors.WithStack(err)
	}

	wrappedCollections := make([]model.PersistedCollection, 0, len(collections))
	for _, c := range collections {
		wrappedCollections = append(wrappedCollections, &wrappedCollection{c})
	}

	return wrappedCollections, nil
}

// ListCollections implements pipeline.CollectionLister.
func (s *Store) ListCollections(ctx context.Context, ids []model.CollectionID) ([]model.Collection, error) {
	persisted, err := s.QueryCollections(ctx, ingest.QueryCollectionsOptions{
		IDs:        ids,
		HeaderOnly: true,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	collections := make([]model.Collection, 0, len(persisted))
	for _, c := range persisted {
		collections = append(collections, c)
	}

	return collections, nil
}

// GetSectionByID implements ingest.Store.
func (s *Store) GetSectionByID(ctx context.Context, id model.SectionID) (model.Section, error) {
	var section Section

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		if err := db.Model(&section).Preload(clause.Associations).First(&section, "id = ?", id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(ingest.ErrNotFound)
			}

			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &wrappedSection{&section}, nil
}

// GetSectionsByIDs implements ingest.Store.
func (s *Store) GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error) {
	if len(ids) == 0 {
		return make(map[model.SectionID]model.Section), nil
	}

	var sections []Section

	rawIDs := make([]string, len(ids))
	for i, id := range ids {
		rawIDs[i] = string(id)
	}

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		if err := db.Preload(clause.Associations).Find(&sections, "id in ?", rawIDs).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	result := make(map[model.SectionID]model.Section, len(sections))
	for i := range sections {
		result[model.SectionID(sections[i].ID)] = &wrappedSection{&sections[i]}
	}

	return result, nil
}

// DeleteDocumentBySource implements ingest.Store.
func (s *Store) DeleteDocumentBySource(ctx context.Context, source *url.URL) error {
	if source == nil {
		return errors.WithStack(ErrMissingSource)
	}

	err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
		var doc Document
		if err := db.First(&doc, "source = ?", source.String()).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}

			return errors.WithStack(err)
		}

		if err := db.Select(clause.Associations).Delete(&doc).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.LOCKED, sqlite3.BUSY)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// QueryDocuments implements ingest.Store.
func (s *Store) QueryDocuments(ctx context.Context, opts ingest.QueryDocumentsOptions) ([]model.PersistedDocument, int64, error) {
	var (
		documents []*Document
		total     int64
	)

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		limit := 10
		if opts.Limit != nil {
			limit = *opts.Limit
		}

		page := 0
		if opts.Page != nil {
			page = *opts.Page
		}

		if err := db.Model(&Document{}).Count(&total).Error; err != nil {
			return errors.WithStack(err)
		}

		query := db.Limit(limit).Offset(page * limit)

		if !opts.HeaderOnly {
			query = query.Preload(clause.Associations).Preload("Sections")
		} else {
			query = query.Omit(clause.Associations).Select("ID", "CreatedAt", "UpdatedAt", "Source", "ETag")
		}

		if opts.MatchingSource != nil {
			query = query.Where("source = ?", opts.MatchingSource.String())
		}

		if opts.Orphaned != nil {
			if *opts.Orphaned {
				// Find documents that have no collections attached
				query = query.Where("id NOT IN (SELECT document_id FROM documents_collections)")
			} else {
				// Find documents that have at least one collection attached
				query = query.Where("id IN (SELECT document_id FROM documents_collections)")
			}
		}

		if err := query.Find(&documents).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return nil, total, errors.WithStack(err)
	}

	wrappedDocuments := make([]model.PersistedDocument, 0, len(documents))
	for _, d := range documents {
		wrappedDocuments = append(wrappedDocuments, &wrappedDocument{d})
	}

	return wrappedDocuments, total, nil
}

// SaveDocuments implements ingest.Store.
func (s *Store) SaveDocuments(ctx context.Context, documents ...model.Document) error {
	for _, doc := range documents {
		err := s.withRetry(ctx, true, func(ctx context.Context, db *gorm.DB) error {
			source := doc.Source()
			if source == nil {
				return errors.WithStack(ErrMissingSource)
			}

			var existing Document
			if err := db.First(&existing, "source = ?", source.String()).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.WithStack(err)
			}

			if existing.ID != "" {
				if err := db.Delete(&existing).Error; err != nil {
					return errors.WithStack(err)
				}
			}

			document, err := fromDocument(doc)
			if err != nil {
				return errors.WithStack(err)
			}

			if res := db.Omit("Sections").Create(document); res.Error != nil {
				return errors.WithStack(res.Error)
			}

			var createSection func(s *Section) error
			createSection = func(s *Section) error {
				err := db.
					Clauses(clause.OnConflict{
						Columns:   []clause.Column{{Name: "id"}},
						UpdateAll: true,
					}).
					Omit("Sections", "Parent", "Document").Create(s).Error
				if err != nil {
					return errors.WithStack(err)
				}

				for _, ss := range s.Sections {
					if err := createSection(ss); err != nil {
						return errors.WithStack(err)
					}

					err := db.Model(&Section{}).
						Where("id = ?", ss.ID).
						Updates(map[string]any{
							"parent_id":   s.ID,
							"document_id": s.DocumentID,
						}).
						Error
					if err != nil {
						return errors.WithStack(err)
					}
				}

				return nil
			}

			for _, s := range document.Sections {
				if err := createSection(s); err != nil {
					return errors.WithStack(err)
				}
			}

			return nil
		}, sqlite3.LOCKED, sqlite3.BUSY)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// QueryDocumentsByCollectionID implements ingest.Store.
func (s *Store) QueryDocumentsByCollectionID(ctx context.Context, collectionID model.CollectionID, opts ingest.QueryDocumentsOptions) ([]model.PersistedDocument, int64, error) {
	var (
		documents []*Document
		total     int64
	)

	err := s.withRetry(ctx, false, func(ctx context.Context, db *gorm.DB) error {
		limit := 10
		if opts.Limit != nil {
			limit = *opts.Limit
		}

		page := 0
		if opts.Page != nil {
			page = *opts.Page
		}

		// Query documents that belong to the specified collection
		// using the many-to-many join table
		query := db.Model(&Document{}).Where(
			"id IN (SELECT document_id FROM documents_collections WHERE collection_id = ?)",
			string(collectionID),
		)

		// Apply source pattern filter before count so total reflects the filter
		if opts.SourcePattern != nil && *opts.SourcePattern != "" {
			query = query.Where("source LIKE ?", "%"+*opts.SourcePattern+"%")
		}

		if err := query.Count(&total).Error; err != nil {
			return errors.WithStack(err)
		}

		// Build ORDER BY from whitelist to prevent SQL injection
		sortColumn := "created_at"
		if opts.SortBy != nil && *opts.SortBy == "source" {
			sortColumn = "source"
		}
		sortDir := "DESC"
		if opts.SortOrder != nil && strings.ToLower(*opts.SortOrder) == "asc" {
			sortDir = "ASC"
		}

		query = query.Limit(limit).Offset(page * limit).Order(sortColumn + " " + sortDir)

		if !opts.HeaderOnly {
			query = query.Preload(clause.Associations).Preload("Sections")
		} else {
			query = query.Omit(clause.Associations).Select("ID", "CreatedAt", "UpdatedAt", "Source", "ETag")
		}

		if err := query.Find(&documents).Error; err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return nil, total, errors.WithStack(err)
	}

	wrappedDocuments := make([]model.PersistedDocument, 0, len(documents))
	for _, d := range documents {
		wrappedDocuments = append(wrappedDocuments, &wrappedDocument{d})
	}

	return wrappedDocuments, total, nil
}
