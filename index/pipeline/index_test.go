package pipeline

import (
	"context"
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

type mockIndex struct {
	indexErr error
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
	return []*index.SearchResult{}, nil
}

var _ index.Index = &mockIndex{}
