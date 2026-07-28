package gorm

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// TestBlobReferences runs the reference-index conformance on SQLite.
func TestBlobReferences(t *testing.T) {
	ctx := context.Background()

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "refs.sqlite"))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer store.Close()

	testBlobReferences(t, ctx, store)
}

// TestBlobReferencesPostgres runs the same conformance on PostgreSQL: the
// index is a derived state maintained by SQL that must behave identically on
// both dialects.
func TestBlobReferencesPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()

	store, err := NewPostgresStore(ctx, startPostgresContainer(t, ctx))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer store.Close()

	testBlobReferences(t, ctx, store)
}

func testBlobReferences(t *testing.T, ctx context.Context, store *Store) {
	// The store must expose the capability, otherwise the cleanup silently
	// falls back to scanning every document.
	if _, ok := any(store).(ingest.BlobReferenceLister); !ok {
		t.Fatal("the store must implement ingest.BlobReferenceLister")
	}

	coll, err := store.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	first := blob.ComputeHash([]byte("first image"))
	second := blob.ComputeHash([]byte("second image"))
	third := blob.ComputeHash([]byte("third image"))

	// The first image is referenced twice in one document and shared with the
	// second document.
	saveDocument(t, ctx, store, coll, "mem:///a",
		"# A\n\n![x]("+blob.URI(first)+")\n\n![x again]("+blob.URI(first)+")\n\n![y]("+blob.URI(second)+")\n")
	docB := saveDocument(t, ctx, store, coll, "mem:///b",
		"# B\n\n![x]("+blob.URI(first)+")\n")

	assertReferenced(t, ctx, store, first, second)

	// Re-indexing document A on a content that dropped the second image must
	// drop the reference with it. Re-indexing yields a new document ID, so the
	// references of the replaced row have to go by source, not by ID.
	docA := saveDocument(t, ctx, store, coll, "mem:///a",
		"# A\n\n![x]("+blob.URI(first)+")\n\n![z]("+blob.URI(third)+")\n")

	assertReferenced(t, ctx, store, first, third)

	// Deleting A leaves B, which still references the shared image.
	if err := store.DeleteDocumentByID(ctx, docA); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	assertReferenced(t, ctx, store, first)

	// A document without image adds nothing.
	saveDocument(t, ctx, store, coll, "mem:///c", "# C\n\nAucune image.\n")

	assertReferenced(t, ctx, store, first)

	// Once the last referencing document is gone, the live set is empty.
	if err := store.DeleteDocumentByID(ctx, docB); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	assertReferenced(t, ctx, store)
}

// TestBlobReferencesMatchContentScan is the differential test the whole design
// hangs on: the derived index must agree, hash for hash, with reading the
// documents. A divergence here is a live blob deleted by the collector.
func TestBlobReferencesMatchContentScan(t *testing.T) {
	ctx := context.Background()

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "refs.sqlite"))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer store.Close()

	coll, err := store.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	hashes := make([]blob.Hash, 0, 4)
	for _, seed := range []string{"a", "b", "c", "d"} {
		hashes = append(hashes, blob.ComputeHash([]byte(seed)))
	}

	contents := []string{
		"# Un\n\n![a](" + blob.URI(hashes[0]) + ")\n",
		"# Deux\n\n![b](" + blob.URI(hashes[1]) + ") ![c](" + blob.URI(hashes[2]) + ")\n",
		"# Trois\n\nAucune image, juste du texte.\n",
		"# Quatre\n\n![a](" + blob.URI(hashes[0]) + ")\n\n![d](" + blob.URI(hashes[3]) + ")\n\n![a](" + blob.URI(hashes[0]) + ")\n",
		"# Cinq\n\n![dangling](amoxtli://images/pas-un-hash) et ![ok](" + blob.URI(hashes[2]) + ")\n",
	}

	for i, content := range contents {
		saveDocument(t, ctx, store, coll, "mem:///doc"+string(rune('a'+i)), content)
	}

	// Expected set: what scanning the stored contents yields.
	expected := map[blob.Hash]struct{}{}
	for _, content := range contents {
		for _, hash := range blob.ScanHashes([]byte(content)) {
			expected[hash] = struct{}{}
		}
	}

	got := map[blob.Hash]struct{}{}
	if err := store.ListReferencedBlobs(ctx, func(hash blob.Hash) error {
		if _, duplicate := got[hash]; duplicate {
			t.Errorf("hash %s enumerated twice", hash)
		}
		got[hash] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(expected) != len(got) {
		t.Fatalf("referenced blobs: expected %d, got %d", len(expected), len(got))
	}

	for hash := range expected {
		if _, exists := got[hash]; !exists {
			t.Errorf("referenced blob %s missing from the index", hash)
		}
	}
}

func TestListReferencedBlobsStopsOnError(t *testing.T) {
	ctx := context.Background()

	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "refs.sqlite"))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer store.Close()

	coll, err := store.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	saveDocument(t, ctx, store, coll, "mem:///a",
		"![x]("+blob.URI(blob.ComputeHash([]byte("x")))+") ![y]("+blob.URI(blob.ComputeHash([]byte("y")))+")\n")

	expected := errors.New("stop")

	calls := 0
	err = store.ListReferencedBlobs(ctx, func(blob.Hash) error {
		calls++
		return expected
	})

	if !errors.Is(err, expected) {
		t.Errorf("expected the callback error, got %+v", err)
	}

	if calls != 1 {
		t.Errorf("expected the walk to stop after 1 call, got %d", calls)
	}
}

func saveDocument(t *testing.T, ctx context.Context, store *Store, coll model.Collection, source, content string) model.DocumentID {
	t.Helper()

	doc, err := markdown.Parse([]byte(content))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	doc.SetSource(parsed)
	doc.AddCollection(coll)

	if err := store.SaveDocuments(ctx, doc); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return doc.ID()
}

func assertReferenced(t *testing.T, ctx context.Context, store *Store, expected ...blob.Hash) {
	t.Helper()

	got := map[blob.Hash]struct{}{}
	if err := store.ListReferencedBlobs(ctx, func(hash blob.Hash) error {
		got[hash] = struct{}{}
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(expected) != len(got) {
		t.Fatalf("referenced blobs: expected %d %v, got %d %v", len(expected), expected, len(got), got)
	}

	for _, hash := range expected {
		if _, exists := got[hash]; !exists {
			t.Errorf("expected blob %s to be referenced", hash)
		}
	}
}
