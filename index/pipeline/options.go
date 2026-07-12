package pipeline

import (
	"context"

	"github.com/bornholm/amoxtli/index"
)

type QueryTransformer interface {
	TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error)
}

type QueryTransformerFunc func(ctx context.Context, query string, opts index.SearchOptions) (string, error)

func (fn QueryTransformerFunc) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	return fn(ctx, query, opts)
}

// SemanticQueryTransformer is an optional marker implemented by query
// transformers that only benefit semantic (vector) indexes, such as HyDE. The
// pipeline applies them solely to indexes reporting index.Semantic() == true,
// leaving lexical indexes to search the untransformed query.
type SemanticQueryTransformer interface {
	QueryTransformer
	// SemanticOnly reports that the transformer must only be applied to
	// semantic (vector) indexes.
	SemanticOnly() bool
}

// isSemanticOnly reports whether t opts into semantic-only application.
func isSemanticOnly(t QueryTransformer) bool {
	s, ok := t.(SemanticQueryTransformer)
	return ok && s.SemanticOnly()
}

type ResultsTransformer interface {
	TransformResults(ctx context.Context, query string, results []*index.SearchResult, opts index.SearchOptions) ([]*index.SearchResult, error)
}

type ResultsTransformerFunc func(ctx context.Context, query string, results []*index.SearchResult, opts index.SearchOptions) ([]*index.SearchResult, error)

func (fn ResultsTransformerFunc) TransformResults(ctx context.Context, query string, results []*index.SearchResult, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return fn(ctx, query, results, opts)
}

type Options struct {
	QueryTransformers   []QueryTransformer
	ResultsTransformers []ResultsTransformer
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		QueryTransformers:   make([]QueryTransformer, 0),
		ResultsTransformers: make([]ResultsTransformer, 0),
	}

	for _, fn := range funcs {
		fn(opts)
	}

	return opts
}

func WithQueryTransformers(transformers ...QueryTransformer) OptionFunc {
	return func(opts *Options) {
		opts.QueryTransformers = transformers
	}
}

func WithResultsTransformers(transformers ...ResultsTransformer) OptionFunc {
	return func(opts *Options) {
		opts.ResultsTransformers = transformers
	}
}
