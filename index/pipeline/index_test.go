package pipeline

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func TestIndex(t *testing.T) {
	index := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("first", &mockIndex{}): 1,
			NewIdentifiedIndex("second", &mockIndex{
				indexErr: errors.New("Oh snap !"),
			}): 1,
			NewIdentifiedIndex("third", &mockIndex{
				indexErr: errors.New("Oh snap !"),
			}): 1,
		},
	)

	document, err := markdown.Parse([]byte(""))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := index.Index(ctx, document); err == nil {
		t.Error("err should not be nil")
	}
}

func TestSearchRespectsMaxResults(t *testing.T) {
	results := make([]*index.SearchResult, 0, 5)
	for n := 0; n < 5; n++ {
		source, err := url.Parse(fmt.Sprintf("mem://doc-%d", n))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		results = append(results, &index.SearchResult{
			Source:   source,
			Sections: []model.SectionID{model.SectionID(fmt.Sprintf("section-%d", n))},
		})
	}

	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("first", &mockIndex{results: results}): 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const maxResults = 3
	got, err := idx.Search(ctx, "query", index.SearchOptions{MaxResults: maxResults})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(got) != maxResults {
		t.Errorf("len(got) = %d, want %d", len(got), maxResults)
	}
}

type mockIndex struct {
	indexErr error
	results  []*index.SearchResult
}

// All implements index.Index.
func (m *mockIndex) All(ctx context.Context, yield func(model.SectionID) bool) error {
	panic("unimplemented")
}

// DeleteByID implements index.Index.
func (m *mockIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	panic("unimplemented")
}

// DeleteBySource implements index.Index.
func (m *mockIndex) DeleteBySource(ctx context.Context, source *url.URL) error {
	return nil
}

// Index implements index.Index.
func (m *mockIndex) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	return m.indexErr
}

// Search implements index.Index.
func (m *mockIndex) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return m.results, nil
}

var _ index.Index = &mockIndex{}
