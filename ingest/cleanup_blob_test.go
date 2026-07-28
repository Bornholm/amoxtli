package ingest

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/blob"
	blobfs "github.com/bornholm/amoxtli/blob/fs"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// TestCleanupOrphanedBlobs checks the garbage collection rule: a blob survives
// as long as one document references it, and only then.
func TestCleanupOrphanedBlobs(t *testing.T) {
	ctx := context.Background()

	blobs := newTestBlobStore(t)

	shared := put(t, blobs, []byte("shared image"))
	single := put(t, blobs, []byte("single image"))
	orphan := put(t, blobs, []byte("orphaned image"))

	store := &blobTestStore{documents: []model.Document{
		// The shared blob is referenced by two documents, the orphan by none.
		newBlobDocument("first", "![A]("+blob.URI(shared)+")\n"),
		newBlobDocument("second", "![A]("+blob.URI(shared)+") ![B]("+blob.URI(single)+")\n"),
	}}

	handler := NewCleanupHandler(&stubIndex{}, store, WithCleanupBlobStore(blobs))

	if err := handler.cleanupOrphanedBlobs(ctx); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	for _, hash := range []blob.Hash{shared, single} {
		if _, _, err := blobs.Get(ctx, hash); err != nil {
			t.Errorf("referenced blob %s must be kept: %+v", hash, err)
		}
	}

	if _, _, err := blobs.Get(ctx, orphan); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("orphaned blob must be collected, got %+v", err)
	}
}

// TestCleanupOrphanedBlobsAfterDocumentRemoval covers the deletion path: the
// document is gone, so the blob it alone referenced becomes collectable.
func TestCleanupOrphanedBlobsAfterDocumentRemoval(t *testing.T) {
	ctx := context.Background()

	blobs := newTestBlobStore(t)
	hash := put(t, blobs, []byte("an image"))

	store := &blobTestStore{documents: []model.Document{
		newBlobDocument("first", "![A]("+blob.URI(hash)+")\n"),
	}}

	handler := NewCleanupHandler(&stubIndex{}, store, WithCleanupBlobStore(blobs))

	if err := handler.cleanupOrphanedBlobs(ctx); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if _, _, err := blobs.Get(ctx, hash); err != nil {
		t.Fatalf("referenced blob must be kept: %+v", err)
	}

	store.documents = nil

	if err := handler.cleanupOrphanedBlobs(ctx); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if _, _, err := blobs.Get(ctx, hash); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("blob of a deleted document must be collected, got %+v", err)
	}
}

// TestCleanupWithoutBlobStore locks that the collection is a no-op when no
// store is configured.
func TestCleanupWithoutBlobStore(t *testing.T) {
	handler := NewCleanupHandler(&stubIndex{}, &blobTestStore{})

	if err := handler.cleanupOrphanedBlobs(context.Background()); err != nil {
		t.Errorf("unexpected error: %+v", err)
	}
}

func newTestBlobStore(t *testing.T) blob.Store {
	t.Helper()

	store, err := blobfs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return store
}

func put(t *testing.T, store blob.Store, data []byte) blob.Hash {
	t.Helper()

	hash, err := store.Put(context.Background(), "image/png", data)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return hash
}

// blobTestStore serves a fixed set of documents; every other Store method
// panics through the embedded nil interface.
type blobTestStore struct {
	Store
	documents []model.Document
}

func (s *blobTestStore) QueryDocuments(ctx context.Context, opts QueryDocumentsOptions) ([]model.PersistedDocument, int64, error) {
	page, limit := 0, len(s.documents)
	if opts.Page != nil {
		page = *opts.Page
	}
	if opts.Limit != nil {
		limit = *opts.Limit
	}

	start := page * limit
	if start >= len(s.documents) {
		return nil, int64(len(s.documents)), nil
	}

	end := min(start+limit, len(s.documents))

	documents := make([]model.PersistedDocument, 0, end-start)
	for _, d := range s.documents[start:end] {
		documents = append(documents, d.(model.PersistedDocument))
	}

	return documents, int64(len(s.documents)), nil
}

func (s *blobTestStore) GetDocumentByID(ctx context.Context, id model.DocumentID) (model.PersistedDocument, error) {
	for _, d := range s.documents {
		if d.ID() == id {
			return d.(model.PersistedDocument), nil
		}
	}

	return nil, errors.Errorf("unknown document '%s'", id)
}

// blobDocument is a minimal model.PersistedDocument carrying content.
type blobDocument struct {
	id      model.DocumentID
	content []byte
}

func newBlobDocument(id string, content string) *blobDocument {
	return &blobDocument{id: model.DocumentID(id), content: []byte(content)}
}

func (d *blobDocument) ID() model.DocumentID     { return d.id }
func (d *blobDocument) Content() ([]byte, error) { return d.content, nil }
func (d *blobDocument) Chunk(start, end int) ([]byte, error) {
	return d.content[start:end], nil
}
func (d *blobDocument) ETag() string                    { return "" }
func (d *blobDocument) Source() *url.URL                { return &url.URL{Scheme: "mem", Path: "/" + string(d.id)} }
func (d *blobDocument) Collections() []model.Collection { return nil }
func (d *blobDocument) Sections() []model.Section       { return nil }
func (d *blobDocument) CreatedAt() time.Time            { return time.Time{} }
func (d *blobDocument) UpdatedAt() time.Time            { return time.Time{} }

var _ model.PersistedDocument = &blobDocument{}
