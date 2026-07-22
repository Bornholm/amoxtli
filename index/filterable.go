package index

import "context"

// FilterableIndex is an optional capability: backends implementing it apply the
// metadata filter inside the search query instead of leaving it to a post-hoc
// pass in Go.
//
// The point is top-k semantics. Filtering after the fact means asking a backend
// for k results and possibly keeping none of them, so the caller has to
// over-fetch and guess how much; a backend that filters inside its query
// returns k results that already satisfy the filter.
//
// # Contract
//
// SearchFiltered behaves exactly like Search, restricted to the sections whose
// document metadata satisfies the filter. An empty (or nil) filter is
// equivalent to Search. SearchOptions.MaxResults bounds the results *after*
// filtering — that is the whole point of the capability.
//
// The filter semantics an implementation must reproduce are specified on
// Filter, and encoded case by case in the shared conformance suite
// index/filtertest. A backend MUST pass that suite before advertising this
// capability: an implementation that disagrees with the Go evaluator on absent
// keys, on int-versus-float or on a date offset would silently return different
// documents than another backend for the same query, and nothing would fail
// loudly.
//
// Implementations may assume the filter keys have been validated (see
// Filter.Validate), which the search pipeline does before calling. They must
// nonetheless treat keys and values as data — bind parameters, never string
// interpolation — since a validated key is not the same thing as a trusted one.
//
// Note the capability is deliberately a dedicated method rather than a Filter
// field on SearchOptions: a backend that ignored such a field would return
// unfiltered results that the caller would believe filtered, which is precisely
// the class of silent corruption the type system should prevent.
type FilterableIndex interface {
	Index

	// SearchFiltered searches like Search, keeping only the sections whose
	// document metadata satisfies filter.
	SearchFiltered(ctx context.Context, query string, filter Filter, opts SearchOptions) ([]*SearchResult, error)
}

// Unwrapper is the convention a decorating Index must follow to stay
// transparent to capability detection.
//
// Capabilities such as FilterableIndex or Semantic are discovered by type
// assertion, which fails against a decorator (logging, metrics, retry) that
// does not itself declare the method. A decorator must therefore either
// re-declare the capability methods it wants to expose, or implement Unwrap so
// the helpers below can look through it — the same pattern as errors.Is/As,
// familiar to Go developers.
type Unwrapper interface {
	// Unwrap returns the decorated index.
	Unwrap() Index
}

// maxUnwrapDepth bounds the unwrap chain. A decorator whose Unwrap returns
// itself (or a cycle of decorators) is a bug, but it must not hang a search:
// past this depth the capability is simply reported as absent.
const maxUnwrapDepth = 32

// capability walks the decorator chain looking for an index implementing T.
func capability[T any](idx Index) (T, bool) {
	for range maxUnwrapDepth {
		if found, ok := idx.(T); ok {
			return found, true
		}

		unwrapper, ok := idx.(Unwrapper)
		if !ok {
			break
		}

		idx = unwrapper.Unwrap()
	}

	var zero T

	return zero, false
}

// ConditionallyFilterable is implemented by indexes whose ability to push the
// filter down depends on their configuration — typically a composite index,
// which can only honour the FilterableIndex contract when every one of its
// legs can.
//
// Declaring SearchFiltered is a static, all-or-nothing claim; this reports the
// runtime answer. It mirrors Semantic, which the pipeline consults the same way.
type ConditionallyFilterable interface {
	// Filterable reports whether SearchFiltered can currently be relied upon.
	Filterable() bool
}

// AsFilterable reports whether idx natively supports filter push-down,
// unwrapping decorators that implement Unwrapper. It is the single detection
// point for the capability: callers must not type-assert FilterableIndex
// directly, or they will miss a decorated backend — or a composite one that
// declares the method but cannot currently honour it.
func AsFilterable(idx Index) (FilterableIndex, bool) {
	filterable, ok := capability[FilterableIndex](idx)
	if !ok {
		return nil, false
	}

	if conditional, ok := filterable.(ConditionallyFilterable); ok && !conditional.Filterable() {
		return nil, false
	}

	return filterable, true
}
