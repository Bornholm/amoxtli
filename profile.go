package amoxtli

import (
	"context"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/internal/stagectx"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/pkg/errors"
)

// SearchProfile is a set of optional stages allowed to run for a search (see
// WithSearchProfile). Configure the Codex with every stage any profile may
// need; each search then picks its profile.
type SearchProfile struct {
	Name   string
	Stages []Stage
}

var (
	// ProfileFast runs no LLM stage: hybrid retrieval and, when configured,
	// the model-free lexical reranker.
	ProfileFast = SearchProfile{Name: "fast"}

	// ProfileBalanced adds query translation for the lexical indexes and the
	// LLM reranker.
	ProfileBalanced = SearchProfile{Name: "balanced", Stages: []Stage{StageTranslate, StageRerank}}

	// ProfileDeep adds, for SearchIterative, query decomposition, iterative
	// reformulation and the grounding evaluator. HyDE is left out of every
	// profile: it measured neutral on the evaluation corpora.
	ProfileDeep = SearchProfile{Name: "deep", Stages: []Stage{StageTranslate, StageRerank, StageDecompose, StageReformulate, StageGrounding}}
)

// stagedReranker runs the LLM reranker when the search allows StageRerank, and
// falls back on the lexical reranker otherwise.
type stagedReranker struct {
	llm     ingest.Reranker
	lexical ingest.Reranker
}

// Rerank implements ingest.Reranker.
func (r *stagedReranker) Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error) {
	reranker := r.lexical
	if r.llm != nil && !stagectx.Skipped(ctx, retrieval.StageRerank) {
		reranker = r.llm
	}

	if reranker == nil {
		return results, nil
	}

	reranked, err := reranker.Rerank(ctx, query, results)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return reranked, nil
}

type iterativeFilterKey struct{}

func withIterativeFilter(ctx context.Context, filter index.Filter) context.Context {
	if len(filter) == 0 {
		return ctx
	}
	return context.WithValue(ctx, iterativeFilterKey{}, filter)
}

func iterativeFilter(ctx context.Context) index.Filter {
	filter, _ := ctx.Value(iterativeFilterKey{}).(index.Filter)
	return filter
}
