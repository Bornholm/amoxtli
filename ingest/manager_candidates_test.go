package ingest

import (
	"context"
	"fmt"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// countingIndex serves the first MaxResults entries of a corpus and records
// every window it was asked for, so a test can assert how the over-fetch loop
// behaved.
type countingIndex struct {
	stubIndex
	corpus  []*index.SearchResult
	windows []int
}

func (c *countingIndex) Search(_ context.Context, _ string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	c.windows = append(c.windows, opts.MaxResults)

	if opts.MaxResults >= len(c.corpus) {
		return c.corpus, nil
	}

	return c.corpus[:opts.MaxResults], nil
}

// countingStore records which sources were asked for, to assert the per-search
// cache does not reload a document already judged.
type countingStore struct {
	stubStore
	requested []string
	calls     int
}

func (s *countingStore) GetDocumentsMetadataBySources(ctx context.Context, sources []string) (map[string]map[string]any, error) {
	s.calls++
	s.requested = append(s.requested, sources...)

	return s.stubStore.GetDocumentsMetadataBySources(ctx, sources)
}

// corpus builds `size` results where one document out of `keepEvery` carries
// lang=fr, along with the matching metadata store.
func corpus(size, keepEvery int) ([]*index.SearchResult, map[string]map[string]any) {
	results := make([]*index.SearchResult, 0, size)
	metadata := map[string]map[string]any{}

	for i := range size {
		host := fmt.Sprintf("doc-%03d", i)
		results = append(results, result(host, float64(size-i)))

		lang := "en"
		if i%keepEvery == 0 {
			lang = "fr"
		}
		metadata["test://"+host] = map[string]any{"lang": lang}
	}

	return results, metadata
}

// Under a selective filter the candidate window must grow until the page can be
// filled, instead of returning a short page from a single fixed-size fetch.
func TestManagerSearchOverFetchesUntilPageIsFull(t *testing.T) {
	results, metadata := corpus(100, 10)
	idx := &countingIndex{corpus: results}
	store := &countingStore{stubStore: stubStore{metadata: metadata}}
	m := newSearchManager(idx, store)

	page, err := m.Search(context.Background(), "q",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Filter{index.Eq("lang", "fr")}))
	if err != nil {
		t.Fatalf("search: %+v", err)
	}

	if len(page.Results) != 5 {
		t.Fatalf("expected a full page of 5 results, got %d", len(page.Results))
	}

	// target=6 (page of 5, plus the lookahead result) → 18, then 54, which
	// holds exactly 6 French documents.
	wantWindows := []int{18, 54}
	if fmt.Sprint(idx.windows) != fmt.Sprint(wantWindows) {
		t.Errorf("candidate windows = %v, want %v", idx.windows, wantWindows)
	}

	// The per-search cache must judge each document exactly once, even though
	// three rounds re-read overlapping prefixes of the same corpus.
	seen := map[string]int{}
	for _, source := range store.requested {
		seen[source]++
	}
	for source, count := range seen {
		if count > 1 {
			t.Errorf("metadata for %s was loaded %d times, want once", source, count)
		}
	}
	if store.calls != len(idx.windows) {
		t.Errorf("metadata was loaded in %d batches, want one per round (%d)", store.calls, len(idx.windows))
	}
}

// The window must depend only on the requested page, never on state carried
// between calls: replaying a search has to fetch exactly the same thing.
func TestManagerSearchCandidateWindowIsDeterministic(t *testing.T) {
	results, metadata := corpus(100, 10)

	windowsFor := func() []int {
		idx := &countingIndex{corpus: results}
		m := newSearchManager(idx, &stubStore{metadata: metadata})

		if _, err := m.Search(context.Background(), "q",
			WithSearchMaxResults(5),
			WithSearchFilter(index.Filter{index.Eq("lang", "fr")})); err != nil {
			t.Fatalf("search: %+v", err)
		}

		return idx.windows
	}

	first, second := windowsFor(), windowsFor()
	if fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("replaying the same search fetched %v then %v", first, second)
	}
}

// A filter matching nothing must terminate on the hard bound rather than
// growing the window indefinitely.
func TestManagerSearchOverFetchStopsAtHardBound(t *testing.T) {
	results, metadata := corpus(1000, 10)
	idx := &countingIndex{corpus: results}
	m := newSearchManager(idx, &stubStore{metadata: metadata})

	page, err := m.Search(context.Background(), "q",
		WithSearchMaxResults(5),
		WithSearchFilter(index.Filter{index.Eq("lang", "unknown")}))
	if err != nil {
		t.Fatalf("search: %+v", err)
	}

	if len(page.Results) != 0 {
		t.Fatalf("expected no result, got %d", len(page.Results))
	}

	last := idx.windows[len(idx.windows)-1]
	if last != maxCandidateFetch {
		t.Errorf("last candidate window = %d, want the hard bound %d", last, maxCandidateFetch)
	}
	for _, w := range idx.windows {
		if w > maxCandidateFetch {
			t.Errorf("candidate window %d exceeds the hard bound %d", w, maxCandidateFetch)
		}
	}
}

// Paginating under a filter must widen the window as the offset grows, so that
// deep pages are reachable instead of being cut off by a fixed pool.
func TestManagerSearchPaginatesUnderFilter(t *testing.T) {
	results, metadata := corpus(100, 10)
	idx := &countingIndex{corpus: results}
	m := newSearchManager(idx, &stubStore{metadata: metadata})
	ctx := context.Background()

	filter := WithSearchFilter(index.Filter{index.Eq("lang", "fr")})

	page1, err := m.Search(ctx, "q", WithSearchMaxResults(3), filter)
	if err != nil {
		t.Fatalf("page1: %+v", err)
	}
	if len(page1.Results) != 3 || page1.NextCursor == "" {
		t.Fatalf("unexpected page1: %d results, cursor %q", len(page1.Results), page1.NextCursor)
	}

	page2, err := m.Search(ctx, "q", WithSearchMaxResults(3), filter, WithSearchCursor(page1.NextCursor))
	if err != nil {
		t.Fatalf("page2: %+v", err)
	}
	if len(page2.Results) != 3 {
		t.Fatalf("expected 3 results on page2, got %d", len(page2.Results))
	}

	// No overlap and no gap: the two pages must be consecutive slices of the
	// same filtered ordering.
	var got []string
	for _, r := range append(append([]*index.SearchResult{}, page1.Results...), page2.Results...) {
		got = append(got, r.Source.Host)
	}
	want := []string{"doc-000", "doc-010", "doc-020", "doc-030", "doc-040", "doc-050"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("paginated sources = %v, want %v", got, want)
	}
}

// The cursor must carry the offset, so a resumed search can size its window
// from offset+pageSize without any server-side state.
func TestSearchCursorCarriesOffset(t *testing.T) {
	results, _ := corpus(10, 1)

	page, cursor, err := paginate(results, "", 3, "")
	if err != nil {
		t.Fatalf("paginate: %+v", err)
	}
	if len(page) != 3 || cursor == "" {
		t.Fatalf("unexpected first page: %d results, cursor %q", len(page), cursor)
	}

	offset, err := resumeCursor(cursor, "")
	if err != nil {
		t.Fatalf("resumeCursor: %+v", err)
	}
	if offset != 3 {
		t.Errorf("cursor offset = %d, want 3 (the next page starts there)", offset)
	}

	if offset, err := resumeCursor("", ""); err != nil || offset != 0 {
		t.Errorf("empty cursor = %d, %v, want 0, nil", offset, err)
	}
}

// A cursor anchors a position inside one filtered ordering. Resuming it under
// another filter would duplicate or skip results, so it must be refused rather
// than silently honoured.
func TestManagerSearchRejectsCursorFromAnotherFilter(t *testing.T) {
	results, metadata := corpus(100, 2)
	m := newSearchManager(&countingIndex{corpus: results}, &stubStore{metadata: metadata})
	ctx := context.Background()

	page1, err := m.Search(ctx, "q", WithSearchMaxResults(3),
		WithSearchFilter(index.Filter{index.Eq("lang", "fr")}))
	if err != nil {
		t.Fatalf("page1: %+v", err)
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a cursor after page1")
	}

	testCases := []struct {
		name    string
		filter  index.Filter
		wantErr bool
	}{
		{"same filter", index.Filter{index.Eq("lang", "fr")}, false},
		{"different value", index.Filter{index.Eq("lang", "en")}, true},
		{"different operator", index.Filter{index.Ne("lang", "fr")}, true},
		{"extra condition", index.Filter{index.Eq("lang", "fr"), index.Exists("lang")}, true},
		{"filter dropped", nil, true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := m.Search(ctx, "q", WithSearchMaxResults(3),
				WithSearchFilter(tc.filter), WithSearchCursor(page1.NextCursor))

			if tc.wantErr {
				if !errors.Is(err, ErrCursorFilterMismatch) {
					t.Fatalf("expected an ErrCursorFilterMismatch, got %+v", err)
				}
				return
			}

			if err != nil {
				t.Fatalf("resuming with the same filter failed: %+v", err)
			}
		})
	}
}

// The fingerprint identifies what a filter selects, not how it was written: an
// equivalent filter must keep paginating.
func TestManagerSearchAcceptsCursorFromEquivalentFilter(t *testing.T) {
	results, metadata := corpus(100, 2)
	for source := range metadata {
		metadata[source]["year"] = 2026.0
	}
	m := newSearchManager(&countingIndex{corpus: results}, &stubStore{metadata: metadata})
	ctx := context.Background()

	page1, err := m.Search(ctx, "q", WithSearchMaxResults(3),
		WithSearchFilter(index.Filter{index.Eq("lang", "fr"), index.Gte("year", 2020)}))
	if err != nil {
		t.Fatalf("page1: %+v", err)
	}

	// Same meaning, different spelling: reordered conditions and an int where a
	// float was used.
	page2, err := m.Search(ctx, "q", WithSearchMaxResults(3),
		WithSearchFilter(index.Filter{index.Gte("year", 2020.0), index.Eq("lang", "fr")}),
		WithSearchCursor(page1.NextCursor))
	if err != nil {
		t.Fatalf("resuming with an equivalent filter failed: %+v", err)
	}

	if len(page2.Results) == 0 {
		t.Fatal("expected page2 to carry results")
	}
	if page2.Results[0].Source.Host == page1.Results[0].Source.Host {
		t.Error("page2 restarted from the beginning instead of resuming")
	}
}

// Cursors of unfiltered searches carry no fingerprint and stay usable.
func TestManagerSearchUnfilteredCursorRoundTrips(t *testing.T) {
	results, _ := corpus(20, 1)
	m := newSearchManager(&countingIndex{corpus: results}, &stubStore{})
	ctx := context.Background()

	page1, err := m.Search(ctx, "q", WithSearchMaxResults(3))
	if err != nil {
		t.Fatalf("page1: %+v", err)
	}

	if _, err := m.Search(ctx, "q", WithSearchMaxResults(3), WithSearchCursor(page1.NextCursor)); err != nil {
		t.Fatalf("resuming an unfiltered search failed: %+v", err)
	}
}

// A search without a filter drops nothing after the fact, so a single round is
// always enough.
func TestManagerSearchWithoutFilterFetchesOnce(t *testing.T) {
	results, _ := corpus(100, 1)
	idx := &countingIndex{corpus: results}
	m := newSearchManager(idx, &stubStore{})

	if _, err := m.Search(context.Background(), "q", WithSearchMaxResults(5)); err != nil {
		t.Fatalf("search: %+v", err)
	}

	if len(idx.windows) != 1 {
		t.Fatalf("expected a single fetch, got windows %v", idx.windows)
	}
}

// An explicit pool size is an override: it must be honoured verbatim, with no
// adaptive growth behind the caller's back.
func TestManagerSearchExplicitPoolSizeIsHonoured(t *testing.T) {
	results, metadata := corpus(100, 10)
	idx := &countingIndex{corpus: results}
	m := newSearchManager(idx, &stubStore{metadata: metadata})

	if _, err := m.Search(context.Background(), "q",
		WithSearchMaxResults(5),
		WithSearchCandidatePoolSize(20),
		WithSearchFilter(index.Filter{index.Eq("lang", "fr")})); err != nil {
		t.Fatalf("search: %+v", err)
	}

	if fmt.Sprint(idx.windows) != fmt.Sprint([]int{20}) {
		t.Fatalf("candidate windows = %v, want [20]", idx.windows)
	}
}
