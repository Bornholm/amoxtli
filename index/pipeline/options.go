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

// LexicalQueryTransformer is the mirror image of SemanticQueryTransformer: an
// optional marker implemented by query transformers that only benefit lexical
// (full-text) indexes, which the pipeline then keeps out of the semantic query.
//
// Query translation is the motivating case. A lexical index cannot cross a
// language barrier at all — no analyzer will match "chien" against "dog" — so
// adding the translated terms to its query is the only remedy. A multilingual
// embedding model, on the other hand, already aligns both languages in one
// space: measured on SciFact, translating French queries recovers 40% of the
// lexical leg's nDCG@10 while the vector leg only ever lost 3%. Feeding the
// translation to the semantic query would pay an LLM call to replace the user's
// question with a paraphrase, for a leg that does not need it.
//
// A transformer must not declare both markers: "only lexical" and "only
// semantic" intersect to nothing, and the pipeline treats such a transformer as
// a configuration error and never applies it (see NewIndex).
type LexicalQueryTransformer interface {
	QueryTransformer
	// LexicalOnly reports that the transformer must only be applied to
	// lexical (full-text) indexes.
	LexicalOnly() bool
}

// isLexicalOnly reports whether t opts into lexical-only application.
func isLexicalOnly(t QueryTransformer) bool {
	l, ok := t.(LexicalQueryTransformer)
	return ok && l.LexicalOnly()
}

// transformerScope tells which query variant a transformer contributes to. It
// is derived from the optional markers, and exists so that the three call sites
// agree on one classification instead of each crossing the predicates itself.
type transformerScope int

const (
	// scopeUniversal contributes to the base query, hence to every leg. It is
	// the default, for a transformer opting into neither marker.
	scopeUniversal transformerScope = iota
	// scopeSemantic contributes to the semantic variant only.
	scopeSemantic
	// scopeLexical contributes to the lexical variant only.
	scopeLexical
	// scopeNone is the contradictory case — both markers declared — which
	// restricts to nothing and is therefore applied nowhere. NewIndex warns
	// about it at wiring time.
	scopeNone
)

func scopeOf(t QueryTransformer) transformerScope {
	semantic, lexical := isSemanticOnly(t), isLexicalOnly(t)

	switch {
	case semantic && lexical:
		return scopeNone
	case semantic:
		return scopeSemantic
	case lexical:
		return scopeLexical
	default:
		return scopeUniversal
	}
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
