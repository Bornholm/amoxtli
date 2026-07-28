package amoxtli

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/blob"
	blobfs "github.com/bornholm/amoxtli/blob/fs"
	"github.com/bornholm/amoxtli/convert"
	convvision "github.com/bornholm/amoxtli/convert/vision"
	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/markdown/imagetext"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

// testPNG128 is a 128x128 PNG: large enough to pass the minimum-dimension
// filter of the embedded-image enrichment.
var testPNG128 = func() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for x := range 128 {
		for y := range 128 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}

	return buf.Bytes()
}()

// 1x1 transparent PNG.
var testPNG = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

// TestCodexIndexImage walks the phase 1 pipeline end to end with a fake
// describer: an image file is converted to markdown, indexed like any other
// document, found by full-text search on a word of its description, and
// selected (or excluded) by the type=image metadata hoisted from the emitted
// frontmatter.
func TestCodexIndexImage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	describer := &fakeDescriber{
		desc: &vision.Description{
			Title:       "Diagramme d'architecture",
			Description: "Un schéma reliant le convertisseur bourguignon à l'index.",
			Text:        "ingestion",
		},
	}

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithFileConverter(convert.NewRouted(convvision.NewConverter(describer))),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("file:///corpus/schema.png")

	taskID, err := codex.IndexFile(ctx, collID, "schema.png", bytes.NewReader(testPNG),
		WithIndexFileSource(source),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	// Found by a word of the description alone: the image itself carries no text.
	results, err := codex.Search(ctx, "convertisseur bourguignon", WithSearchMaxResults(5))
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Fatal("len(results): expected the image to be found by its description")
	}

	if e, g := source.String(), results[0].Source.String(); e != g {
		t.Errorf("results[0].Source.String(): expected %s, got %s", e, g)
	}

	// type=image is hoisted from the frontmatter emitted by the converter.
	filtered, err := codex.Search(ctx, "convertisseur bourguignon",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Eq("type", "image")),
	)
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(filtered) == 0 {
		t.Error("len(results) with --filter type=image: expected the image to be kept")
	}

	excluded, err := codex.Search(ctx, "convertisseur bourguignon",
		WithSearchMaxResults(5),
		WithSearchFilter(index.NotExists("type")),
	)
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(excluded) != 0 {
		t.Errorf("len(results) with --filter '!type': expected the image to be excluded, got %d", len(excluded))
	}
}

// TestCodexIndexImageUserMetadataWins locks the merge order: metadata supplied
// at ingestion time override the ones hoisted from the frontmatter.
func TestCodexIndexImageUserMetadataWins(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	describer := &fakeDescriber{
		desc: &vision.Description{Description: "Un schéma reliant le convertisseur bourguignon à l'index."},
	}

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithFileConverter(convert.NewRouted(convvision.NewConverter(describer))),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("file:///corpus/schema.png")

	taskID, err := codex.IndexFile(ctx, collID, "schema.png", bytes.NewReader(testPNG),
		WithIndexFileSource(source),
		WithIndexFileMetadata(map[string]any{"type": "diagram"}),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	results, err := codex.Search(ctx, "convertisseur bourguignon",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Eq("type", "diagram")),
	)
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Error("len(results) with --filter type=diagram: expected the user metadata to win over the frontmatter")
	}

	results, err = codex.Search(ctx, "convertisseur bourguignon",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Eq("type", "image")),
	)
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) != 0 {
		t.Errorf("len(results) with --filter type=image: expected the frontmatter value to be overridden, got %d", len(results))
	}
}

func waitForTask(t *testing.T, codex *Codex, taskID task.ID) {
	t.Helper()

	ctx := context.Background()
	deadline := time.Now().Add(30 * time.Second)

	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			t.Fatalf("could not get task state: %+v", errors.WithStack(err))
		}

		if state.Status == task.StatusSucceeded {
			return
		}

		if state.Status == task.StatusFailed {
			t.Fatalf("indexing task failed: %+v", state.Error)
		}

		if time.Now().After(deadline) {
			t.Fatalf("indexing task did not finish in time (status: %s)", state.Status)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// fakeDescriber replays a canned description without any LLM call.
type fakeDescriber struct {
	desc  *vision.Description
	calls int
}

func (d *fakeDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*vision.Description, error) {
	d.calls++
	return d.desc, nil
}

var _ vision.Describer = &fakeDescriber{}

func mustDecodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}

	return decoded
}

// TestCodexIndexMarkdownWithEmbeddedImage walks the phase 2 pipeline end to
// end: a markdown document embedding an image is enriched with its
// description before parsing, so the document is retrieved by a word of that
// description — while the image itself is still stripped from the rendered
// chunks.
func TestCodexIndexMarkdownWithEmbeddedImage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	describer := &fakeDescriber{
		desc: &vision.Description{
			Title:       "Diagramme",
			Description: "Un schéma reliant le convertisseur bourguignon à l'index.",
		},
	}

	// The cache is what makes re-indexing free: the second document embeds the
	// very same image, described only once.
	cached, err := vision.NewCachingDescriber(describer, filepath.Join(dir, "cache"), "test")
	if err != nil {
		t.Fatalf("could not create description cache: %+v", errors.WithStack(err))
	}

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithImageEnrichment(imagetext.WithDescriber(cached)),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	document := "# Architecture\n\n![Schéma](data:image/png;base64," +
		base64.StdEncoding.EncodeToString(testPNG128) + ")\n"

	source, _ := url.Parse("file:///corpus/note.md")

	taskID, err := codex.IndexFile(ctx, collID, "note.md", strings.NewReader(document),
		WithIndexFileSource(source),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	results, err := codex.Search(ctx, "convertisseur bourguignon", WithSearchMaxResults(5))
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Fatal("len(results): expected the document to be found by the image description")
	}

	sections, err := codex.GetSectionsByIDs(ctx, results[0].Sections)
	if err != nil {
		t.Fatalf("could not get sections: %+v", errors.WithStack(err))
	}

	var content string
	for _, section := range sections {
		chunk, err := section.Content()
		if err != nil {
			t.Fatalf("could not read section: %+v", errors.WithStack(err))
		}
		content += string(chunk)
	}

	if !strings.Contains(content, "convertisseur bourguignon") {
		t.Errorf("section content: expected the image description, got:\n%s", content)
	}

	// Re-indexing the same image — under another document — costs no LLM call.
	otherSource, _ := url.Parse("file:///corpus/autre.md")

	taskID, err = codex.IndexFile(ctx, collID, "autre.md", strings.NewReader(document),
		WithIndexFileSource(otherSource),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	if describer.calls != 1 {
		t.Errorf("describer.calls after re-indexing: expected 1 (cache hit), got %d", describer.calls)
	}
}

// TestCodexIndexImageWithBlobStore closes the loop of phase 5: an indexed image
// is not only findable by its description, it is fetchable again through the
// internal URI carried by the section content.
func TestCodexIndexImageWithBlobStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	blobs, err := blobfs.NewStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("could not open blob store: %+v", errors.WithStack(err))
	}

	describer := &fakeDescriber{
		desc: &vision.Description{
			Title:       "Diagramme",
			Description: "Un schéma reliant le convertisseur bourguignon à l'index.",
		},
	}

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithFileConverter(convert.NewRouted(
			convvision.NewConverterWithOptions(describer, nil, []convvision.Option{
				convvision.WithBlobStore(blobs),
			}),
		)),
		WithBlobStore(blobs),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	if codex.Blobs() == nil {
		t.Fatal("Blobs(): expected the configured store")
	}

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("file:///corpus/schema.png")

	taskID, err := codex.IndexFile(ctx, collID, "schema.png", bytes.NewReader(testPNG),
		WithIndexFileSource(source),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	hash := blob.ComputeHash(testPNG)

	// The image itself was stored, byte for byte.
	stored, info, err := blobs.Get(ctx, hash)
	if err != nil {
		t.Fatalf("could not read the stored image: %+v", errors.WithStack(err))
	}

	if !bytes.Equal(testPNG, stored) {
		t.Error("stored blob: expected the original image bytes")
	}

	if e, g := "image/png", info.MimeType; e != g {
		t.Errorf("stored blob mime type: expected %q, got %q", e, g)
	}

	// And the section content handed to a consumer carries the reference.
	results, err := codex.Search(ctx, "convertisseur bourguignon", WithSearchMaxResults(5))
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Fatal("len(results): expected the image to be found by its description")
	}

	sections, err := codex.GetSectionsByIDs(ctx, results[0].Sections)
	if err != nil {
		t.Fatalf("could not get sections: %+v", errors.WithStack(err))
	}

	var content string
	for _, section := range sections {
		chunk, err := section.Content()
		if err != nil {
			t.Fatalf("could not read section: %+v", errors.WithStack(err))
		}
		content += string(chunk)
	}

	if !strings.Contains(content, blob.URI(hash)) {
		t.Errorf("section content: expected the internal URI, got:\n%s", content)
	}

	// Backup carries the blobs along with the documents: restoring documents
	// without them would leave dead references.
	snapshot, err := codex.Backup(ctx)
	if err != nil {
		t.Fatalf("could not backup: %+v", errors.WithStack(err))
	}
	defer snapshot.Close()

	if err := codex.Restore(ctx, snapshot); err != nil {
		t.Fatalf("could not restore: %+v", errors.WithStack(err))
	}

	if _, _, err := blobs.Get(ctx, hash); err != nil {
		t.Errorf("the image must survive a backup/restore roundtrip: %+v", err)
	}
}

// TestCodexCleanupCollectsOrphanedBlobs exercises the blob garbage collection
// through the real store, which serves the live set from its reference index
// (ingest.BlobReferenceLister) rather than by reading every document.
func TestCodexCleanupCollectsOrphanedBlobs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	blobs, err := blobfs.NewStore(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatalf("could not open blob store: %+v", errors.WithStack(err))
	}

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithBlobStore(blobs),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	referenced, err := blobs.Put(ctx, "image/png", testPNG)
	if err != nil {
		t.Fatalf("could not store image: %+v", errors.WithStack(err))
	}

	orphan, err := blobs.Put(ctx, "image/png", []byte("never referenced"))
	if err != nil {
		t.Fatalf("could not store image: %+v", errors.WithStack(err))
	}

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("file:///corpus/note.md")
	document := "# Diagramme\n\n![Schéma](" + blob.URI(referenced) + ")\n"

	taskID, err := codex.IndexFile(ctx, collID, "note.md", strings.NewReader(document),
		WithIndexFileSource(source),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	taskID, err = codex.CleanupIndex(ctx)
	if err != nil {
		t.Fatalf("could not schedule cleanup: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	if _, _, err := blobs.Get(ctx, referenced); err != nil {
		t.Errorf("a referenced blob must survive the cleanup: %+v", err)
	}

	if _, _, err := blobs.Get(ctx, orphan); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("an unreferenced blob must be collected, got %+v", err)
	}

	// Once the document is gone, its image becomes collectable in turn.
	if err := codex.DeleteBySource(ctx, source); err != nil {
		t.Fatalf("could not delete document: %+v", errors.WithStack(err))
	}

	taskID, err = codex.CleanupIndex(ctx)
	if err != nil {
		t.Fatalf("could not schedule cleanup: %+v", errors.WithStack(err))
	}

	waitForTask(t, codex, taskID)

	if _, _, err := blobs.Get(ctx, referenced); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the blob of a deleted document must be collected, got %+v", err)
	}
}
