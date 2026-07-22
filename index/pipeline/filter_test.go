package pipeline

import (
	"context"
	"net/url"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/index"
)

// filterableMock is a mockIndex that also applies the filter itself.
type filterableMock struct {
	mockIndex

	mu       sync.Mutex
	received []index.Filter
}

func (m *filterableMock) SearchFiltered(ctx context.Context, query string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	m.mu.Lock()
	m.received = append(m.received, filter)
	m.mu.Unlock()

	return m.Search(ctx, query, opts)
}

func (m *filterableMock) filters() []index.Filter {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]index.Filter{}, m.received...)
}

func mockResult(host string) *index.SearchResult {
	return &index.SearchResult{Source: &url.URL{Scheme: "test", Host: host}, Score: 1}
}

// The pipeline merges its legs' results, so it can only promise a fully
// filtered list when every leg filters. One plain leg is enough to make the
// merged list carry unfiltered results, which the caller would wrongly trust.
func TestPipelineFilterableRequiresEveryLeg(t *testing.T) {
	filterable := func() *filterableMock {
		return &filterableMock{mockIndex: mockIndex{results: []*index.SearchResult{mockResult("a")}}}
	}
	plain := func() *mockIndex {
		return &mockIndex{results: []*index.SearchResult{mockResult("b")}}
	}

	testCases := []struct {
		name    string
		indexes WeightedIndexes
		want    bool
	}{
		{"no index", WeightedIndexes{}, false},
		{"single filterable leg", WeightedIndexes{NewIdentifiedIndex("a", filterable()): 1}, true},
		{"single plain leg", WeightedIndexes{NewIdentifiedIndex("a", plain()): 1}, false},
		{
			name: "every leg filterable",
			indexes: WeightedIndexes{
				NewIdentifiedIndex("a", filterable()): 1,
				NewIdentifiedIndex("b", filterable()): 1,
			},
			want: true,
		},
		{
			name: "one plain leg among filterable ones",
			indexes: WeightedIndexes{
				NewIdentifiedIndex("a", filterable()): 1,
				NewIdentifiedIndex("b", plain()):      1,
			},
			want: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pipeline := NewIndex(tc.indexes)

			if got := pipeline.Filterable(); got != tc.want {
				t.Errorf("Filterable() = %v, want %v", got, tc.want)
			}

			// The detection helper must agree: it is the only thing callers use.
			if _, got := index.AsFilterable(pipeline); got != tc.want {
				t.Errorf("index.AsFilterable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// When the pipeline does push down, every leg must receive the filter.
func TestPipelinePushesFilterToEveryLeg(t *testing.T) {
	first := &filterableMock{mockIndex: mockIndex{results: []*index.SearchResult{mockResult("a")}}}
	second := &filterableMock{mockIndex: mockIndex{results: []*index.SearchResult{mockResult("b")}}}

	pipeline := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("first", first):   1,
		NewIdentifiedIndex("second", second): 1,
	})

	filter := index.Filter{index.Eq("lang", "fr")}

	results, err := pipeline.SearchFiltered(context.Background(), "q", filter, index.SearchOptions{MaxResults: 10})
	if err != nil {
		t.Fatalf("SearchFiltered: %+v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected both legs to contribute, got %d results", len(results))
	}

	for name, leg := range map[string]*filterableMock{"first": first, "second": second} {
		got := leg.filters()
		if len(got) != 1 {
			t.Errorf("leg %s received %d filters, want 1", name, len(got))
			continue
		}
		if len(got[0]) != len(filter) || got[0][0] != filter[0] {
			t.Errorf("leg %s received filter %v, want %v", name, got[0], filter)
		}
	}
}

// An empty filter must leave the legs on their plain Search path.
func TestPipelineEmptyFilterUsesPlainSearch(t *testing.T) {
	leg := &filterableMock{mockIndex: mockIndex{results: []*index.SearchResult{mockResult("a")}}}
	pipeline := NewIndex(WeightedIndexes{NewIdentifiedIndex("a", leg): 1})

	if _, err := pipeline.SearchFiltered(context.Background(), "q", nil, index.SearchOptions{MaxResults: 10}); err != nil {
		t.Fatalf("SearchFiltered: %+v", err)
	}

	if got := leg.filters(); len(got) != 0 {
		t.Errorf("leg received %v for an empty filter, want no push-down", got)
	}
}

// Asking a pipeline that cannot push down to do so must fail loudly rather than
// return a partially filtered list.
func TestPipelineSearchFilteredRefusesWhenNotFilterable(t *testing.T) {
	pipeline := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("a", &filterableMock{mockIndex: mockIndex{results: []*index.SearchResult{mockResult("a")}}}): 1,
		NewIdentifiedIndex("b", &mockIndex{results: []*index.SearchResult{mockResult("b")}}):                            1,
	})

	if _, err := pipeline.SearchFiltered(context.Background(), "q",
		index.Filter{index.Eq("lang", "fr")}, index.SearchOptions{MaxResults: 10}); err == nil {
		t.Fatal("expected an error when a leg cannot apply the filter")
	}
}
