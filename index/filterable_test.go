package index_test

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
)

// plainIndex implements index.Index and nothing else.
type plainIndex struct{}

func (*plainIndex) Index(context.Context, model.Document, ...index.OptionFunc) error { return nil }
func (*plainIndex) DeleteBySource(context.Context, *url.URL) error                   { return nil }
func (*plainIndex) DeleteByID(context.Context, ...model.SectionID) error             { return nil }
func (*plainIndex) All(context.Context, func(model.SectionID) bool) error            { return nil }
func (*plainIndex) Search(context.Context, string, index.SearchOptions) ([]*index.SearchResult, error) {
	return nil, nil
}

// filterableIndex additionally declares the push-down capability.
type filterableIndex struct{ plainIndex }

func (*filterableIndex) SearchFiltered(context.Context, string, index.Filter, index.SearchOptions) ([]*index.SearchResult, error) {
	return nil, nil
}

// semanticIndex additionally declares the vector-search capability.
type semanticIndex struct{ plainIndex }

func (*semanticIndex) Semantic() bool { return true }

// The decorators below embed plainIndex to satisfy index.Index and keep the
// decorated index in a named field: index.Index cannot be embedded, since its
// Index method would collide with the embedded field of the same name.

// opaque wraps an index without exposing what it wraps: the decorator
// anti-pattern the Unwrapper convention exists to prevent.
type opaqueDecorator struct {
	plainIndex
	wrapped index.Index
}

func opaque(wrapped index.Index) *opaqueDecorator { return &opaqueDecorator{wrapped: wrapped} }

// unwrapping follows the convention.
type unwrappingDecorator struct {
	plainIndex
	wrapped index.Index
}

func (d *unwrappingDecorator) Unwrap() index.Index { return d.wrapped }

func unwrapping(wrapped index.Index) *unwrappingDecorator {
	return &unwrappingDecorator{wrapped: wrapped}
}

// selfWrappingDecorator is buggy: its Unwrap never makes progress.
type selfWrappingDecorator struct{ plainIndex }

func (d *selfWrappingDecorator) Unwrap() index.Index { return d }

func TestAsFilterable(t *testing.T) {
	filterable := &filterableIndex{}

	testCases := []struct {
		name string
		idx  index.Index
		want bool
	}{
		{"plain index", &plainIndex{}, false},
		{"filterable index", filterable, true},
		{"nil index", nil, false},
		{"behind an unwrapping decorator", unwrapping(filterable), true},
		{"behind two unwrapping decorators", unwrapping(unwrapping(filterable)), true},
		{"behind an opaque decorator", opaque(filterable), false},
		{"unwrapping decorator over a plain index", unwrapping(&plainIndex{}), false},
		{"opaque decorator under an unwrapping one", unwrapping(opaque(filterable)), false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := index.AsFilterable(tc.idx)

			if ok != tc.want {
				t.Fatalf("AsFilterable() = _, %v, want %v", ok, tc.want)
			}

			if ok && got == nil {
				t.Fatal("AsFilterable() reported the capability but returned a nil index")
			}
			if !ok && got != nil {
				t.Fatalf("AsFilterable() reported no capability but returned %#v", got)
			}
		})
	}
}

// A decorator whose Unwrap does not make progress is a bug, but it must not
// hang the search: the capability is simply reported as absent.
func TestAsFilterableTerminatesOnCyclicDecorator(t *testing.T) {
	done := make(chan struct{})

	go func() {
		defer close(done)

		if _, ok := index.AsFilterable(&selfWrappingDecorator{}); ok {
			t.Error("a cyclic decorator should not resolve to a capability")
		}
	}()

	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("AsFilterable did not terminate on a cyclic decorator")
	}
}

// Capability detection must look through decorators uniformly, not just for
// FilterableIndex.
func TestIsSemanticUnwrapsDecorators(t *testing.T) {
	semantic := &semanticIndex{}

	testCases := []struct {
		name string
		idx  index.Index
		want bool
	}{
		{"plain index", &plainIndex{}, false},
		{"semantic index", semantic, true},
		{"behind an unwrapping decorator", unwrapping(semantic), true},
		{"behind an opaque decorator", opaque(semantic), false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := index.IsSemantic(tc.idx); got != tc.want {
				t.Fatalf("IsSemantic() = %v, want %v", got, tc.want)
			}
		})
	}
}
