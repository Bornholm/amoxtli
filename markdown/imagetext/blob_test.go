package imagetext

import (
	"context"
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/blob"
	blobfs "github.com/bornholm/amoxtli/blob/fs"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

func TestEnrichRewritesDestinationToBlobURI(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "schema.png"), 128, 128)

	store := newBlobStore(t)

	const document = `# Architecture

![Schéma](media/schema.png)

Du texte.
`

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma."}}

	enriched := string(enrich(t, []byte(document),
		WithDescriber(describer),
		WithBaseDir(dir),
		WithBlobStore(store),
	))

	hash := storedHash(t, store)

	if !strings.Contains(enriched, "![Schéma]("+blob.URI(hash)+")") {
		t.Errorf("enriched document: expected the internal URI, got:\n%s", enriched)
	}

	if strings.Contains(enriched, "media/schema.png") {
		t.Errorf("enriched document: expected the local path rewritten, got:\n%s", enriched)
	}

	if !strings.Contains(enriched, "> **Image** : Un schéma.") {
		t.Errorf("enriched document: expected the description, got:\n%s", enriched)
	}
}

// TestEnrichBlobURISurvivesTrim is the point of the whole scheme: unlike a data
// URI, the internal URI is still there in the chunk handed to an agent.
func TestEnrichBlobURISurvivesTrim(t *testing.T) {
	data := imageBytes(t, 128, 128)
	store := newBlobStore(t)

	document := "# Titre\n\n![Schéma](data:image/png;base64," + base64.StdEncoding.EncodeToString(data) + ")\n"

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma."}}

	enriched := enrich(t, []byte(document),
		WithDescriber(describer),
		WithBlobStore(store),
	)

	if strings.Contains(string(enriched), "data:image/png;base64,") {
		t.Errorf("enriched document: expected the data URI replaced, got:\n%s", enriched)
	}

	doc, err := markdown.Parse(enriched)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	content, err := doc.Sections()[0].Content()
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if !strings.Contains(string(content), blob.URI(storedHash(t, store))) {
		t.Errorf("rendered chunk: expected the internal URI to survive, got:\n%s", content)
	}

	if strings.Contains(string(content), "#stripped") {
		t.Errorf("rendered chunk: expected no stripped destination, got:\n%s", content)
	}
}

func TestEnrichWithoutDescriberStoresAndRewrites(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	store := newBlobStore(t)

	// A blob store alone is a legitimate setup: reference the images without
	// paying a single LLM call.
	enriched := string(enrich(t, []byte("![Schéma](schema.png)\n"),
		WithBaseDir(dir),
		WithBlobStore(store),
	))

	if !strings.Contains(enriched, blob.URI(storedHash(t, store))) {
		t.Errorf("enriched document: expected the internal URI, got:\n%s", enriched)
	}

	if strings.Contains(enriched, "> **Image**") {
		t.Errorf("enriched document: expected no description, got:\n%s", enriched)
	}
}

func TestEnrichSharesOneBlobAcrossOccurrences(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "a.png"), 128, 128)
	writeImage(t, filepath.Join(dir, "copie.png"), 128, 128)

	store := newBlobStore(t)

	document := "![A](a.png)\n\n![Copie](copie.png)\n"

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	enriched := string(enrich(t, []byte(document),
		WithDescriber(describer),
		WithBaseDir(dir),
		WithBlobStore(store),
	))

	// Same bytes under two names: one blob, referenced twice.
	uri := blob.URI(storedHash(t, store))

	if e, g := 2, strings.Count(enriched, uri); e != g {
		t.Errorf("references to the blob: expected %d, got %d in:\n%s", e, g, enriched)
	}
}

func TestInlineLocalImagesStoresInsteadOfInlining(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "image1.png"), 64, 64)

	store := newBlobStore(t)

	inlined, err := InlineLocalImagesWithStore(context.Background(),
		[]byte("![Schéma](media/image1.png)\n"), dir, 0, store)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if strings.Contains(string(inlined), "base64,") {
		t.Errorf("inlined document: expected no data URI when a store is given, got:\n%s", inlined)
	}

	if !strings.Contains(string(inlined), blob.URI(storedHash(t, store))) {
		t.Errorf("inlined document: expected the internal URI, got:\n%s", inlined)
	}
}

func TestInlineLocalImagesFallsBackToDataURI(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "image1.png"), 64, 64)

	inlined, err := InlineLocalImagesWithStore(context.Background(),
		[]byte("![Schéma](media/image1.png)\n"), dir, 0, nil)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if !strings.Contains(string(inlined), "data:image/png;base64,") {
		t.Errorf("inlined document: expected a data URI without a store, got:\n%s", inlined)
	}
}

func TestEnrichKeepsReferenceWhenStoreFails(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	document := []byte("![Schéma](schema.png)\n")

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma."}}

	enriched := string(enrich(t, document,
		WithDescriber(describer),
		WithBaseDir(dir),
		WithBlobStore(failingBlobStore{}),
	))

	// The description was paid for: it must land even though the store failed.
	if !strings.Contains(enriched, "> **Image** : Un schéma.") {
		t.Errorf("enriched document: expected the description, got:\n%s", enriched)
	}

	if !strings.Contains(enriched, "![Schéma](schema.png)") {
		t.Errorf("enriched document: expected the original reference kept, got:\n%s", enriched)
	}
}

func newBlobStore(t *testing.T) blob.Store {
	t.Helper()

	store, err := blobfs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return store
}

// storedHash returns the hash of the single blob in the store.
func storedHash(t *testing.T, store blob.Store) blob.Hash {
	t.Helper()

	var hashes []blob.Hash

	if err := store.List(context.Background(), func(info blob.Info) error {
		hashes = append(hashes, info.Hash)
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(hashes) != 1 {
		t.Fatalf("expected exactly one stored blob, got %d", len(hashes))
	}

	return hashes[0]
}

// failingBlobStore refuses every write.
type failingBlobStore struct{ blob.Store }

func (failingBlobStore) Put(ctx context.Context, mimeType string, data []byte) (blob.Hash, error) {
	return "", errors.New("store unavailable")
}
