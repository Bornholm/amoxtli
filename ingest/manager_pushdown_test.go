package ingest

import (
	"context"
	"fmt"
	"testing"

	"github.com/bornholm/amoxtli/index"
)

// filterableIndex serves a corpus, applying the metadata filter itself the way
// a real filterable backend does: the top-k is picked among matching documents.
type filterableIndex struct {
	countingIndex
	metadata map[string]map[string]any
	filters  []index.Filter
}

func (f *filterableIndex) SearchFiltered(_ context.Context, _ string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	f.filters = append(f.filters, filter)
	f.windows = append(f.windows, opts.MaxResults)

	kept := make([]*index.SearchResult, 0, opts.MaxResults)
	for _, r := range f.corpus {
		if !filter.Matches(f.metadata[sourceKey(r.Source)]) {
			continue
		}

		kept = append(kept, r)
		if len(kept) == opts.MaxResults {
			break
		}
	}

	return kept, nil
}

// A filterable index must be handed the filter, and its results taken as-is:
// no over-fetching loop and no metadata reload behind its back.
func TestManagerSearchPushesFilterDown(t *testing.T) {
	results, metadata := corpus(100, 10)
	idx := &filterableIndex{countingIndex: countingIndex{corpus: results}, metadata: metadata}
	store := &countingStore{stubStore: stubStore{metadata: metadata}}
	m := newSearchManager(idx, store)

	filter := index.Filter{index.Eq("lang", "fr")}

	page, err := m.Search(context.Background(), "q", WithSearchMaxResults(5), WithSearchFilter(filter))
	if err != nil {
		t.Fatalf("search: %+v", err)
	}

	if len(page.Results) != 5 {
		t.Fatalf("expected a full page of 5, got %d", len(page.Results))
	}

	if len(idx.filters) != 1 {
		t.Fatalf("expected one SearchFiltered call, got %d", len(idx.filters))
	}
	if fmt.Sprint(idx.filters[0]) != fmt.Sprint(filter) {
		t.Errorf("index received filter %v, want %v", idx.filters[0], filter)
	}

	// One shot: the whole point of push-down is that k results are k matching
	// results, so there is nothing to grow towards.
	if len(idx.windows) != 1 {
		t.Errorf("expected a single fetch, got windows %v", idx.windows)
	}

	// The store must not be consulted at all: re-checking the index's work
	// would defeat the purpose and cost a query per page.
	if store.calls != 0 {
		t.Errorf("metadata was reloaded %d time(s) despite push-down", store.calls)
	}
}

// The observable result must not depend on where the filter was applied. This
// is the invariant the whole design protects: same corpus, same filter,
// push-down and Go fallback must return the same documents in the same order.
func TestManagerSearchPushdownMatchesFallback(t *testing.T) {
	ctx := context.Background()

	filters := []struct {
		name   string
		filter index.Filter
	}{
		{"equality", index.Filter{index.Eq("lang", "fr")}},
		{"absent key", index.Filter{index.NotExists("lang")}},
		{"inequality", index.Filter{index.Ne("lang", "fr")}},
		{"range", index.Filter{index.Gte("rank", 50)}},
		{"conjunction", index.Filter{index.Eq("lang", "fr"), index.Gte("rank", 30)}},
		{"matches nothing", index.Filter{index.Eq("lang", "unknown")}},
	}

	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			results, metadata := corpus(100, 3)
			// Give half the documents a rank, and strip lang from a few so the
			// absent-key cases are exercised too.
			for i, r := range results {
				key := sourceKey(r.Source)
				if i%2 == 0 {
					metadata[key]["rank"] = float64(i)
				}
				if i%7 == 0 {
					delete(metadata[key], "lang")
				}
			}

			pushdown := newSearchManager(
				&filterableIndex{countingIndex: countingIndex{corpus: results}, metadata: metadata},
				&stubStore{metadata: metadata})
			fallback := newSearchManager(
				&countingIndex{corpus: results},
				&stubStore{metadata: metadata})

			pushed, err := pushdown.Search(ctx, "q", WithSearchMaxResults(7), WithSearchFilter(tc.filter))
			if err != nil {
				t.Fatalf("push-down search: %+v", err)
			}

			filteredInGo, err := fallback.Search(ctx, "q", WithSearchMaxResults(7), WithSearchFilter(tc.filter))
			if err != nil {
				t.Fatalf("fallback search: %+v", err)
			}

			if got, want := resultSources(pushed.Results), resultSources(filteredInGo.Results); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("push-down returned %v, fallback returned %v", got, want)
			}
		})
	}
}

// Without a filter, a filterable index must be searched exactly as before.
func TestManagerSearchUnfilteredIgnoresPushdown(t *testing.T) {
	results, metadata := corpus(20, 2)
	idx := &filterableIndex{countingIndex: countingIndex{corpus: results}, metadata: metadata}
	m := newSearchManager(idx, &stubStore{metadata: metadata})

	if _, err := m.Search(context.Background(), "q", WithSearchMaxResults(5)); err != nil {
		t.Fatalf("search: %+v", err)
	}

	if len(idx.filters) != 0 {
		t.Errorf("SearchFiltered was called for an unfiltered search: %v", idx.filters)
	}
}

// Push-down must not lift the hard bound on how much a single search fetches.
func TestManagerSearchPushdownRespectsHardBound(t *testing.T) {
	results, metadata := corpus(2000, 1)
	idx := &filterableIndex{countingIndex: countingIndex{corpus: results}, metadata: metadata}
	m := newSearchManager(idx, &stubStore{metadata: metadata})

	// A cursor deep enough that offset+pageSize alone would exceed the bound.
	cursor, err := encodeCursor(results[0], maxCandidateFetch*2, filterFingerprint(index.Filter{index.Eq("lang", "fr")}))
	if err != nil {
		t.Fatalf("encodeCursor: %+v", err)
	}

	if _, err := m.Search(context.Background(), "q",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Filter{index.Eq("lang", "fr")}),
		WithSearchCursor(cursor)); err != nil {
		t.Fatalf("search: %+v", err)
	}

	for _, w := range idx.windows {
		if w > maxCandidateFetch {
			t.Errorf("push-down fetched %d, above the hard bound %d", w, maxCandidateFetch)
		}
	}
}

func resultSources(results []*index.SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Source.Host)
	}
	return out
}
