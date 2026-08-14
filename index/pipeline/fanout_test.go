package pipeline

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// TestSearchSurvivesPanickingLeg pins the failure mode the per-leg result
// channel used to have: a leg that panicked never sent its message, so the
// collecting loop waited forever for it — a search that hung instead of
// failing. The panic must now come back as an error, and quickly.
func TestSearchSurvivesPanickingLeg(t *testing.T) {
	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("healthy", &mockIndex{}):        1,
		NewIdentifiedIndex("panicking", &panickingIndex{}): 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := idx.Search(ctx, "query", index.SearchOptions{MaxResults: 3})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when a leg panics")
		}
		if !strings.Contains(err.Error(), "boom") {
			t.Errorf("error = %v, want it to carry the panic value", err)
		}
	case <-ctx.Done():
		t.Fatal("Search never returned: a panicking leg must not deadlock the fan-out")
	}
}

// TestDeleteBySourceSurvivesPanickingLeg covers the same fan-out on the write
// path, where the panic recovery did not even send a message.
func TestDeleteBySourceSurvivesPanickingLeg(t *testing.T) {
	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("healthy", &mockIndex{}):        1,
		NewIdentifiedIndex("panicking", &panickingIndex{}): 1,
	})

	source, err := url.Parse("mem://doc")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- idx.DeleteBySource(ctx, source) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when a leg panics")
		}
	case <-ctx.Done():
		t.Fatal("DeleteBySource never returned: a panicking leg must not deadlock the fan-out")
	}
}

// TestFanOutAggregatesEveryLegError checks that a failure in one leg does not
// hide the failure in another: both must be reported.
func TestFanOutAggregatesEveryLegError(t *testing.T) {
	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("first", &mockIndex{indexErr: errors.New("first failed")}):   1,
		NewIdentifiedIndex("second", &mockIndex{indexErr: errors.New("second failed")}): 1,
	})

	document, err := markdown.Parse([]byte(""))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err = idx.Index(ctx, document)
	if err == nil {
		t.Fatal("expected an error")
	}

	for _, want := range []string{"first failed", "second failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// panickingIndex panics on every operation, standing in for a backend with a
// latent bug (a nil map write, an out-of-range slice).
type panickingIndex struct{}

func (p *panickingIndex) All(ctx context.Context, yield func(model.SectionID) bool) error {
	panic("boom")
}

func (p *panickingIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	panic("boom")
}

func (p *panickingIndex) DeleteBySource(ctx context.Context, source *url.URL) error {
	panic("boom")
}

func (p *panickingIndex) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	panic("boom")
}

func (p *panickingIndex) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	panic("boom")
}

var _ index.Index = &panickingIndex{}
