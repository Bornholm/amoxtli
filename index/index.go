package index

import (
	"context"
	"net/url"

	"github.com/bornholm/amoxtli/model"
)

type Index interface {
	Index(ctx context.Context, document model.Document, funcs ...OptionFunc) error
	DeleteBySource(ctx context.Context, source *url.URL) error
	DeleteByID(ctx context.Context, ids ...model.SectionID) error
	All(ctx context.Context, yield func(model.SectionID) bool) error
	Search(ctx context.Context, query string, opts SearchOptions) ([]*SearchResult, error)
}

type Options struct {
	OnProgress func(progress float32)
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithOnProgress(onProgress func(progress float32)) OptionFunc {
	return func(opts *Options) {
		opts.OnProgress = onProgress
	}
}

type SearchOptions struct {
	MaxResults  int
	Collections []model.CollectionID
}

type SearchResult struct {
	Source   *url.URL
	Sections []model.SectionID
	// Score is the relevance score of the result (higher is better). Its scale
	// is backend-specific and only meaningful for ranking results within the
	// same Search response — not as an absolute confidence across queries or
	// backends.
	Score float64
	// SectionScores holds the per-section relevance scores when the backend
	// exposes them, keyed by section ID. It may be nil.
	SectionScores map[model.SectionID]float64
}

// Semantic is an optional capability implemented by indexes performing
// embedding/vector similarity search, which therefore benefit from query
// expansion such as HyDE. Full-text (lexical) indexes must not implement it —
// or must return false — so the search pipeline queries them with the raw
// query. Hybrid indexes that manage their own lexical/vector fusion internally
// should also report false, to avoid polluting their lexical leg with an
// expanded query.
type Semantic interface {
	// Semantic reports whether the index performs vector similarity search.
	Semantic() bool
}

// IsSemantic reports whether idx declares itself as a semantic (vector) index
// via the Semantic capability.
func IsSemantic(idx Index) bool {
	s, ok := idx.(Semantic)
	return ok && s.Semantic()
}
