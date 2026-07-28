package gorm

import (
	"context"

	"github.com/bornholm/amoxtli/blob"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DocumentBlob records that a document references a blob. It is a derived
// index over the content of the documents, maintained in the same transaction
// as the write that produced it — which is what keeps it from drifting away
// from its source of truth.
//
// It exists so the cleanup task can compute the set of live blobs with a
// single indexed query instead of reading every document. Without it the sweep
// costs a full pass over the corpus; the trade is one small row per image
// reference.
type DocumentBlob struct {
	DocumentID string `gorm:"primaryKey;size:64"`
	Hash       string `gorm:"primaryKey;size:64;index"`

	// The foreign key deletes the references along with their document, so a
	// document removed by any path — including one that bypasses
	// DeleteDocumentByID — cannot leave stale references behind.
	Document *Document `gorm:"foreignKey:DocumentID;constraint:OnDelete:CASCADE"`
}

// TableName pins the table name so it does not depend on gorm's pluralization.
func (DocumentBlob) TableName() string {
	return "document_blobs"
}

// saveBlobReferences replaces the references of a document with the ones its
// content carries. It must run inside the transaction that writes the
// document: an interrupted save then leaves neither the document nor its
// references.
func saveBlobReferences(db *gorm.DB, document *Document) error {
	if err := deleteBlobReferences(db, document.ID); err != nil {
		return errors.WithStack(err)
	}

	hashes := blob.ScanHashes(document.Content)
	if len(hashes) == 0 {
		return nil
	}

	// One row per (document, blob): the same image referenced twice in a
	// document is one reference.
	seen := make(map[blob.Hash]struct{}, len(hashes))
	references := make([]DocumentBlob, 0, len(hashes))

	for _, hash := range hashes {
		if _, duplicate := seen[hash]; duplicate {
			continue
		}

		seen[hash] = struct{}{}

		references = append(references, DocumentBlob{
			DocumentID: document.ID,
			Hash:       string(hash),
		})
	}

	err := db.
		Clauses(clause.OnConflict{DoNothing: true}).
		Omit("Document").
		Create(&references).Error
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// deleteBlobReferences drops every reference held by the given documents.
func deleteBlobReferences(db *gorm.DB, documentIDs ...string) error {
	if len(documentIDs) == 0 {
		return nil
	}

	if err := db.Where("document_id IN ?", documentIDs).Delete(&DocumentBlob{}).Error; err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// referencedBlobsBatchSize bounds how many hashes are held in memory at once
// while walking the live set.
const referencedBlobsBatchSize = 500

// ListReferencedBlobs implements ingest.BlobReferenceLister: it walks the
// distinct blob hashes referenced by at least one stored document — the live
// set of the blob garbage collector.
func (s *Store) ListReferencedBlobs(ctx context.Context, fn func(blob.Hash) error) error {
	db, err := s.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	var lastHash string

	for {
		if err := ctx.Err(); err != nil {
			return errors.WithStack(err)
		}

		var hashes []string

		query := db.WithContext(ctx).
			Model(&DocumentBlob{}).
			Distinct("hash").
			Order("hash asc").
			Limit(referencedBlobsBatchSize)

		if lastHash != "" {
			query = query.Where("hash > ?", lastHash)
		}

		if err := query.Pluck("hash", &hashes).Error; err != nil {
			return errors.WithStack(err)
		}

		if len(hashes) == 0 {
			return nil
		}

		for _, hash := range hashes {
			if err := fn(blob.Hash(hash)); err != nil {
				return errors.WithStack(err)
			}
		}

		lastHash = hashes[len(hashes)-1]
	}
}
