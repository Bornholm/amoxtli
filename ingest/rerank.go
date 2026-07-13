package ingest

import (
	"context"

	"github.com/bornholm/amoxtli/index"
)

// Reranker reorders search results by relevance to the query, refining the
// initial retrieval/fusion ranking. It is an optional component plugged into
// the search pipeline (see Manager and WithManagerReranker). Implementations
// may reorder results and their sections and adjust scores, but must not
// fabricate new results. It runs after metadata filtering and before
// pagination, so the reranked order is the one exposed to callers and encoded
// in pagination cursors.
type Reranker interface {
	Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error)
}
