package sqlitevec

import (
	"context"
	"fmt"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/filtertest"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/pkg/errors"
)

// TestFilterConformance runs the shared semantics suite against the SQL
// translation. Passing it is what lets this index advertise
// index.FilterableIndex: a document kept by the Go evaluator must be kept by
// the pushed-down filter, and conversely — otherwise the same query would
// return different documents depending on the backend, silently.
func TestFilterConformance(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	// Every case gets its own document: the suite asks whether *this* metadata
	// satisfies the filter, so the document must be identifiable among the
	// others already indexed.
	var indexed int

	filtertest.Run(t, func(ctx context.Context, filter index.Filter, metadata map[string]any) (bool, error) {
		indexed++
		source := fmt.Sprintf("mem://case-%03d", indexed)

		if err := indexWithMetadata(ctx, idx, source, metadata); err != nil {
			return false, errors.WithStack(err)
		}

		// maxResults is well above the corpus so the KNN never truncates: what
		// the assertion measures must be the filter, not the ranking.
		results, err := idx.SearchFiltered(ctx, "a question about ALPHA", filter, index.SearchOptions{MaxResults: 1000})
		if err != nil {
			return false, errors.WithStack(err)
		}

		for _, r := range results {
			if r.Source.String() == source {
				return true, nil
			}
		}

		return false, nil
	})

	_ = ctx
}

func indexWithMetadata(ctx context.Context, idx *Index, source string, metadata map[string]any) error {
	doc, err := markdown.Parse([]byte("# Case\n\nThis section is about ALPHA topics.\n"))
	if err != nil {
		return errors.WithStack(err)
	}

	u, err := url.Parse(source)
	if err != nil {
		return errors.WithStack(err)
	}

	doc.SetSource(u)
	doc.SetMetadata(metadata)

	return errors.WithStack(idx.Index(ctx, doc))
}

// The capability must be discoverable the way the pipeline discovers it.
func TestIndexAdvertisesFilterable(t *testing.T) {
	idx := newHermeticIndex(t, &keywordClient{})

	if _, ok := index.AsFilterable(idx); !ok {
		t.Fatal("sqlitevec index does not advertise index.FilterableIndex")
	}
}

// A filter must restrict the KNN itself, not trim its output: with a selective
// filter, the k results returned have to be k *matching* results.
func TestSearchFilteredKeepsTopK(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	// 20 documents, all equally relevant to the query; only the last 5 carry
	// lang=fr. A post-hoc filter over a top-5 would return nothing.
	for n := range 20 {
		lang := "en"
		if n >= 15 {
			lang = "fr"
		}
		source := fmt.Sprintf("mem://doc-%02d", n)
		if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": lang}); err != nil {
			t.Fatalf("indexing %s: %+v", source, err)
		}
	}

	results, err := idx.SearchFiltered(ctx, "a question about ALPHA",
		index.Filter{index.Eq("lang", "fr")}, index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("SearchFiltered: %+v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 matching results, got %d: %+v", len(results), sources(results))
	}
	for _, r := range results {
		if r.Source.String() < "mem://doc-15" {
			t.Errorf("result %s does not satisfy the filter", r.Source)
		}
	}
}

// An empty filter must behave exactly like Search.
func TestSearchFilteredWithoutFilterMatchesSearch(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")
	indexDoc(t, ctx, idx, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n")

	plain, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("Search: %+v", err)
	}

	for _, filter := range []index.Filter{nil, {}} {
		filtered, err := idx.SearchFiltered(ctx, "a question about ALPHA", filter, index.SearchOptions{MaxResults: 5})
		if err != nil {
			t.Fatalf("SearchFiltered(%#v): %+v", filter, err)
		}

		if fmt.Sprint(sources(filtered)) != fmt.Sprint(sources(plain)) {
			t.Errorf("SearchFiltered(%#v) = %v, want %v", filter, sources(filtered), sources(plain))
		}
	}
}

// Re-indexing a document must refresh its metadata copy, and deleting it must
// drop it: a stale copy would let a filter match a document this index no
// longer holds, or hold differently.
func TestDocumentMetadataFollowsTheDocument(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	source := "mem://doc"
	matches := func(filter index.Filter) bool {
		results, err := idx.SearchFiltered(ctx, "a question about ALPHA", filter, index.SearchOptions{MaxResults: 10})
		if err != nil {
			t.Fatalf("SearchFiltered: %+v", err)
		}
		return len(results) > 0
	}

	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "fr"}); err != nil {
		t.Fatalf("index: %+v", err)
	}
	if !matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Fatal("freshly indexed metadata is not filterable")
	}

	// Re-index with different metadata.
	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "en"}); err != nil {
		t.Fatalf("reindex: %+v", err)
	}
	if matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Error("stale metadata survived a re-index")
	}
	if !matches(index.Filter{index.Eq("lang", "en")}) {
		t.Error("refreshed metadata is not filterable")
	}

	// Re-index with no metadata at all.
	if err := indexWithMetadata(ctx, idx, source, nil); err != nil {
		t.Fatalf("reindex without metadata: %+v", err)
	}
	if matches(index.Filter{index.Exists("lang")}) {
		t.Error("metadata survived a re-index that carried none")
	}

	// Delete the document.
	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "fr"}); err != nil {
		t.Fatalf("reindex: %+v", err)
	}
	u, _ := url.Parse(source)
	if err := idx.DeleteBySource(ctx, u); err != nil {
		t.Fatalf("delete: %+v", err)
	}
	if matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Error("metadata survived the deletion of its document")
	}
}

// Metadata is part of what the index holds, so a backup/restore cycle must
// bring it back: restoring an index that can no longer be filtered would be a
// silent data loss.
func TestSnapshotPreservesDocumentMetadata(t *testing.T) {
	ctx := context.Background()
	source := &keywordClient{}

	origin := newHermeticIndex(t, source)
	if err := indexWithMetadata(ctx, origin, "mem://doc", map[string]any{"lang": "fr", "year": 2026.0}); err != nil {
		t.Fatalf("index: %+v", err)
	}

	snapshot, err := origin.GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %+v", err)
	}
	defer snapshot.Close()

	restored := newHermeticIndex(t, source)
	if err := restored.RestoreSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("RestoreSnapshot: %+v", err)
	}

	results, err := restored.SearchFiltered(ctx, "a question about ALPHA",
		index.Filter{index.Eq("lang", "fr"), index.Gte("year", 2020)}, index.SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatalf("SearchFiltered on the restored index: %+v", err)
	}

	if len(results) != 1 || results[0].Source.String() != "mem://doc" {
		t.Fatalf("restored index lost its metadata: got %v", sources(results))
	}
}

// A filter key outside index.KeyPattern must be refused rather than reaching
// the JSON path builder, where it could raise a SQL error.
func TestSearchFilteredRejectsInvalidKey(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")

	_, err := idx.SearchFiltered(ctx, "a question about ALPHA",
		index.Filter{index.Eq("'; DROP TABLE embeddings; --", "x")}, index.SearchOptions{MaxResults: 5})
	if !errors.Is(err, index.ErrInvalidFilterKey) {
		t.Fatalf("expected an ErrInvalidFilterKey, got %+v", err)
	}

	// The table must obviously still be there.
	if _, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("index damaged by the rejected filter: %+v", err)
	}
}

func sources(results []*index.SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Source.String())
	}
	return out
}
