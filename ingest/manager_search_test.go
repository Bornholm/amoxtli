package ingest

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// stubIndex implements index.Index, returning a canned result list. (The
// interface is implemented explicitly rather than embedded because index.Index
// has a method named Index, which would collide with an embedded field.)
type stubIndex struct {
	results []*index.SearchResult
}

func (s *stubIndex) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return s.results, nil
}

func (s *stubIndex) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	return nil
}
func (s *stubIndex) DeleteBySource(ctx context.Context, source *url.URL) error       { return nil }
func (s *stubIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error    { return nil }
func (s *stubIndex) All(ctx context.Context, yield func(model.SectionID) bool) error { return nil }

var _ index.Index = &stubIndex{}

// stubStore implements ingest.Store (unused methods panic via the embedded nil
// interface) and, optionally, MetadataProvider.
type stubStore struct {
	Store
	metadata map[string]map[string]any
}

func (s *stubStore) GetDocumentsMetadataBySources(ctx context.Context, sources []string) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, src := range sources {
		if m, ok := s.metadata[src]; ok {
			out[src] = m
		}
	}
	return out, nil
}

func result(host string, score float64, sections ...model.SectionID) *index.SearchResult {
	return &index.SearchResult{
		Source:   &url.URL{Scheme: "test", Host: host},
		Sections: sections,
		Score:    score,
	}
}

func newSearchManager(idx index.Index, store Store) *Manager {
	return &Manager{Store: store, index: idx}
}

func TestManagerSearchCursorPagination(t *testing.T) {
	results := []*index.SearchResult{
		result("a", 5, "sa"),
		result("b", 4, "sb"),
		result("c", 3, "sc"),
		result("d", 2, "sd"),
		result("e", 1, "se"),
	}
	m := newSearchManager(&stubIndex{results: results}, &stubStore{})
	ctx := context.Background()

	// Force pagination over the candidate pool with a small page size.
	page1, err := m.Search(ctx, "q", WithSearchMaxResults(2), WithSearchCursor(""))
	if err != nil {
		t.Fatalf("page1: %+v", err)
	}
	// A cursor forces the larger pool; here we drive pagination via page size,
	// so trigger the pool by requesting a cursor-based flow explicitly.
	if len(page1.Results) != 2 || page1.Results[0].Source.Host != "a" || page1.Results[1].Source.Host != "b" {
		t.Fatalf("unexpected page1: %+v", page1.Results)
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a next cursor after page1")
	}

	page2, err := m.Search(ctx, "q", WithSearchMaxResults(2), WithSearchCursor(page1.NextCursor))
	if err != nil {
		t.Fatalf("page2: %+v", err)
	}
	if len(page2.Results) != 2 || page2.Results[0].Source.Host != "c" || page2.Results[1].Source.Host != "d" {
		t.Fatalf("unexpected page2: %+v", page2.Results)
	}

	page3, err := m.Search(ctx, "q", WithSearchMaxResults(2), WithSearchCursor(page2.NextCursor))
	if err != nil {
		t.Fatalf("page3: %+v", err)
	}
	if len(page3.Results) != 1 || page3.Results[0].Source.Host != "e" {
		t.Fatalf("unexpected page3: %+v", page3.Results)
	}
	if page3.NextCursor != "" {
		t.Fatalf("expected no cursor on last page, got %q", page3.NextCursor)
	}
}

func TestManagerSearchMetadataFilter(t *testing.T) {
	results := []*index.SearchResult{
		result("a", 3, "sa"),
		result("b", 2, "sb"),
		result("c", 1, "sc"),
	}
	store := &stubStore{metadata: map[string]map[string]any{
		"test://a": {"lang": "fr", "year": float64(2026)},
		"test://b": {"lang": "en", "year": float64(2025)},
		"test://c": {"lang": "fr", "year": float64(2024)},
	}}
	m := newSearchManager(&stubIndex{results: results}, store)
	ctx := context.Background()

	page, err := m.Search(ctx, "q", WithSearchMaxResults(10), WithSearchFilter(index.Filter{index.Eq("lang", "fr")}))
	if err != nil {
		t.Fatalf("search: %+v", err)
	}
	if len(page.Results) != 2 {
		t.Fatalf("expected 2 fr results, got %d", len(page.Results))
	}
	for _, r := range page.Results {
		if r.Source.Host != "a" && r.Source.Host != "c" {
			t.Fatalf("unexpected filtered result: %s", r.Source.Host)
		}
	}

	// Range condition on a numeric metadata value.
	page, err = m.Search(ctx, "q", WithSearchMaxResults(10), WithSearchFilter(index.Filter{index.Gte("year", 2025)}))
	if err != nil {
		t.Fatalf("search range: %+v", err)
	}
	if len(page.Results) != 2 {
		t.Fatalf("expected 2 results with year>=2025, got %d", len(page.Results))
	}
}

// Filter keys reach the manager from caller-facing surfaces, so an invalid one
// must be rejected before it is evaluated — let alone handed to a backend.
func TestManagerSearchRejectsInvalidFilterKey(t *testing.T) {
	store := &stubStore{metadata: map[string]map[string]any{"test://a": {"lang": "fr"}}}
	m := newSearchManager(&stubIndex{results: []*index.SearchResult{result("a", 1, "sa")}}, store)

	_, err := m.Search(context.Background(), "q", WithSearchFilter(index.Filter{index.Eq("'; DROP TABLE documents; --", "x")}))
	if !errors.Is(err, index.ErrInvalidFilterKey) {
		t.Fatalf("expected an ErrInvalidFilterKey, got %+v", err)
	}
}

func TestManagerSearchFilterRequiresProvider(t *testing.T) {
	// A store that does NOT implement MetadataProvider must fail loudly when a
	// filter is requested.
	type bareStore struct{ Store }
	m := newSearchManager(&stubIndex{results: []*index.SearchResult{result("a", 1, "sa")}}, &bareStore{})

	_, err := m.Search(context.Background(), "q", WithSearchFilter(index.Filter{index.Eq("k", "v")}))
	if err == nil {
		t.Fatal("expected an error when filtering without a MetadataProvider store")
	}
}

// reverseReranker reverses the result order, to assert rerank runs before
// pagination.
type reverseReranker struct{}

func (reverseReranker) Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error) {
	out := make([]*index.SearchResult, len(results))
	for i, r := range results {
		out[len(results)-1-i] = r
	}
	return out, nil
}

func TestManagerSearchRerankBeforePagination(t *testing.T) {
	results := []*index.SearchResult{
		result("a", 3, "sa"),
		result("b", 2, "sb"),
		result("c", 1, "sc"),
	}
	m := newSearchManager(&stubIndex{results: results}, &stubStore{})
	m.reranker = reverseReranker{}

	page, err := m.Search(context.Background(), "q", WithSearchMaxResults(2))
	if err != nil {
		t.Fatalf("search: %+v", err)
	}
	if len(page.Results) != 2 || page.Results[0].Source.Host != "c" || page.Results[1].Source.Host != "b" {
		t.Fatalf("expected reranked (reversed) order c,b, got %+v", page.Results)
	}
}
