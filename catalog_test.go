package amoxtli

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func indexSync(t *testing.T, codex *Codex, collID model.CollectionID, rawSource string, metadata map[string]any) {
	t.Helper()

	source, _ := url.Parse(rawSource)
	if err := codex.IndexFileSync(context.Background(), collID, "doc.md", strings.NewReader(testDocument),
		WithIndexFileSource(source), WithIndexFileMetadata(metadata)); err != nil {
		t.Fatalf("IndexFileSync: %+v", errors.WithStack(err))
	}
}

func documentIDBySource(t *testing.T, codex *Codex, collID model.CollectionID, rawSource string) model.DocumentID {
	t.Helper()

	documents, _, err := codex.ListDocuments(context.Background(), collID, ListDocumentsOptions{})
	if err != nil {
		t.Fatalf("ListDocuments: %+v", errors.WithStack(err))
	}
	for _, d := range documents {
		if d.Source().String() == rawSource {
			return d.ID()
		}
	}
	t.Fatalf("document %s not found", rawSource)
	return ""
}

func searchSources(t *testing.T, codex *Codex, funcs ...SearchOption) []string {
	t.Helper()

	results, err := codex.Search(context.Background(), "bourguignon", funcs...)
	if err != nil {
		t.Fatalf("Search: %+v", errors.WithStack(err))
	}
	sources := make([]string, 0, len(results))
	for _, r := range results {
		sources = append(sources, r.Source.String())
	}
	return sources
}

func TestCodexCollectionsCatalog(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	label, description := "Recettes", "Recettes du terroir"
	if _, err := codex.UpdateCollection(ctx, collID, CollectionUpdates{Label: &label, Description: &description}); err != nil {
		t.Fatalf("UpdateCollection: %+v", errors.WithStack(err))
	}

	collection, err := codex.GetCollection(ctx, collID)
	if err != nil {
		t.Fatalf("GetCollection: %+v", errors.WithStack(err))
	}
	if collection.Label() != label || collection.Description() != description {
		t.Errorf("unexpected collection %q / %q", collection.Label(), collection.Description())
	}

	otherID, err := codex.CreateCollection(ctx, "other")
	if err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}

	collections, err := codex.ListCollections(ctx)
	if err != nil {
		t.Fatalf("ListCollections: %+v", errors.WithStack(err))
	}
	if len(collections) != 2 {
		t.Errorf("expected 2 collections, got %d", len(collections))
	}

	indexSync(t, codex, collID, "https://example.net/a.md", nil)
	indexSync(t, codex, collID, "https://example.net/b.md", nil)

	documents, total, err := codex.ListDocuments(ctx, collID, ListDocumentsOptions{Limit: 1})
	if err != nil {
		t.Fatalf("ListDocuments: %+v", errors.WithStack(err))
	}
	if total != 2 || len(documents) != 1 {
		t.Errorf("expected 1 document of 2, got %d of %d", len(documents), total)
	}

	stats, err := codex.CollectionStats(ctx, collID)
	if err != nil {
		t.Fatalf("CollectionStats: %+v", errors.WithStack(err))
	}
	if stats.TotalDocuments != 2 {
		t.Errorf("expected 2 documents in stats, got %d", stats.TotalDocuments)
	}

	if err := codex.DeleteCollection(ctx, otherID); err != nil {
		t.Fatalf("DeleteCollection: %+v", errors.WithStack(err))
	}
	if _, err := codex.GetCollection(ctx, otherID); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for a deleted collection, got %v", err)
	}
}

func TestCodexDeleteDocument(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	indexSync(t, codex, collID, "https://example.net/a.md", nil)
	id := documentIDBySource(t, codex, collID, "https://example.net/a.md")

	if err := codex.DeleteDocument(ctx, id); err != nil {
		t.Fatalf("DeleteDocument: %+v", errors.WithStack(err))
	}

	if sources := searchSources(t, codex, WithSearchCollections(collID)); len(sources) != 0 {
		t.Errorf("deleted document still found: %v", sources)
	}
	if _, err := codex.GetDocument(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCodexUpdateDocumentMetadata(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	indexSync(t, codex, collID, "https://example.net/a.md", map[string]any{"type": "brouillon"})
	id := documentIDBySource(t, codex, collID, "https://example.net/a.md")

	filter := WithSearchFilter(index.Eq("type", "recette"))

	if sources := searchSources(t, codex, WithSearchCollections(collID), filter); len(sources) != 0 {
		t.Fatalf("unexpected match before the update: %v", sources)
	}

	if err := codex.UpdateDocumentMetadata(ctx, id, map[string]any{"type": "recette"}); err != nil {
		t.Fatalf("UpdateDocumentMetadata: %+v", errors.WithStack(err))
	}

	if sources := searchSources(t, codex, WithSearchCollections(collID), filter); len(sources) != 1 {
		t.Errorf("expected the updated document to match, got %v", sources)
	}
}

func TestCodexSetDocumentCollections(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	otherID, err := codex.CreateCollection(ctx, "other")
	if err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}

	indexSync(t, codex, collID, "https://example.net/a.md", nil)
	id := documentIDBySource(t, codex, collID, "https://example.net/a.md")

	if err := codex.SetDocumentCollections(ctx, id, otherID); err != nil {
		t.Fatalf("SetDocumentCollections: %+v", errors.WithStack(err))
	}

	if sources := searchSources(t, codex, WithSearchCollections(collID)); len(sources) != 0 {
		t.Errorf("document still found in its former collection: %v", sources)
	}
	if sources := searchSources(t, codex, WithSearchCollections(otherID)); len(sources) != 1 {
		t.Errorf("document not found in its new collection: %v", sources)
	}
}

func TestCodexListDocumentsFilter(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	indexSync(t, codex, collID, "https://example.net/a.md", map[string]any{"type": "contrat", "tags": []any{"bail", "loyer"}})
	indexSync(t, codex, collID, "https://example.net/b.md", map[string]any{"type": "note"})
	indexSync(t, codex, collID, "https://example.net/c.md", map[string]any{"type": "contrat"})

	documents, total, err := codex.ListDocuments(ctx, collID, ListDocumentsOptions{Filter: index.Filter{index.Eq("type", "contrat")}, Limit: 1})
	if err != nil {
		t.Fatalf("ListDocuments: %+v", errors.WithStack(err))
	}
	if total != 2 || len(documents) != 1 {
		t.Errorf("expected 1 document of 2, got %d of %d", len(documents), total)
	}

	second, _, err := codex.ListDocuments(ctx, collID, ListDocumentsOptions{Filter: index.Filter{index.Eq("type", "contrat")}, Limit: 1, Page: 1})
	if err != nil || len(second) != 1 || second[0].ID() == documents[0].ID() {
		t.Errorf("expected the other contract on the second page, got %v %+v", second, err)
	}

	tagged, total, err := codex.ListDocuments(ctx, collID, ListDocumentsOptions{Filter: index.Filter{index.Any("tags", "loyer")}})
	if err != nil || total != 1 || len(tagged) != 1 || tagged[0].Source().String() != "https://example.net/a.md" {
		t.Errorf("expected the tagged document only, got %d %+v", total, err)
	}

	if _, _, err := codex.ListDocuments(ctx, collID, ListDocumentsOptions{Filter: index.Filter{index.Eq("bad key", "x")}}); err == nil {
		t.Error("expected an invalid filter key to be rejected")
	}
}
