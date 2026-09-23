package index

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/model"
)

// stubIndex implements Index with no-op methods.
type stubIndex struct{}

func (stubIndex) Index(ctx context.Context, document model.Document, funcs ...OptionFunc) error {
	return nil
}
func (stubIndex) DeleteBySource(ctx context.Context, source *url.URL) error       { return nil }
func (stubIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error    { return nil }
func (stubIndex) All(ctx context.Context, yield func(model.SectionID) bool) error { return nil }
func (stubIndex) Search(ctx context.Context, query string, opts SearchOptions) ([]*SearchResult, error) {
	return nil, nil
}

type metadataRecorder struct {
	stubIndex
	updated map[string]any
}

func (r *metadataRecorder) UpdateMetadata(ctx context.Context, source *url.URL, metadata map[string]any) error {
	r.updated = metadata
	return nil
}

type unwrappingIndex struct {
	stubIndex
	inner Index
}

func (w *unwrappingIndex) Unwrap() Index { return w.inner }

func TestUpdateMetadataUnwraps(t *testing.T) {
	recorder := &metadataRecorder{}
	source, _ := url.Parse("https://example.net/doc")

	if err := UpdateMetadata(context.Background(), &unwrappingIndex{inner: recorder}, source, map[string]any{"k": "v"}); err != nil {
		t.Fatalf("UpdateMetadata: %+v", err)
	}
	if recorder.updated["k"] != "v" {
		t.Errorf("the wrapped updater was not reached: %v", recorder.updated)
	}

	if err := UpdateMetadata(context.Background(), stubIndex{}, source, map[string]any{"k": "v"}); err != nil {
		t.Errorf("an index without metadata copy must be a no-op, got %+v", err)
	}
}
