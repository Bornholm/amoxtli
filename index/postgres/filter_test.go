package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/filtertest"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

const filterTestBody = "# Case\n\nThis section is about metadata filtering.\n"

const filterTestQuery = "metadata filtering"

// TestFilterConformance runs the shared semantics suite against the JSONB
// translation. Passing it is what lets this index advertise
// index.FilterableIndex: a document kept by the Go evaluator must be kept by
// the pushed-down filter, and conversely — otherwise the same query would
// return different documents depending on the backend, silently.
//
// It runs on the full-text leg alone, which needs no embeddings model: the
// filter translation is shared by both legs.
func TestFilterConformance(t *testing.T) {
	idx := NewIndex(requirePostgres(t), nil)

	var indexed int

	filtertest.Run(t, func(ctx context.Context, filter index.Filter, metadata map[string]any) (bool, error) {
		indexed++
		source := fmt.Sprintf("mem://case-%03d", indexed)

		if err := indexWithMetadata(ctx, idx, source, metadata); err != nil {
			return false, errors.WithStack(err)
		}

		// The limit is well above the corpus so ranking never truncates: what
		// the assertion measures must be the filter alone.
		results, err := idx.SearchFiltered(ctx, filterTestQuery, filter, index.SearchOptions{MaxResults: 1000})
		if err != nil {
			return false, errors.WithStack(err)
		}

		for _, r := range results {
			if r.Source.String() == source {
				return true, nil
			}
		}

		return false, nil
	})
}

// The capability must be discoverable the way the pipeline discovers it.
func TestIndexAdvertisesFilterable(t *testing.T) {
	if _, ok := index.AsFilterable(NewIndex(nil, nil)); !ok {
		t.Fatal("postgres index does not advertise index.FilterableIndex")
	}
}

// The filter must restrict each leg before its LIMIT, not trim the fused
// output: with a selective filter, the k results returned have to be k
// *matching* results.
func TestSearchFilteredKeepsTopK(t *testing.T) {
	pool := requirePostgres(t)
	ctx := context.Background()

	idx := NewIndex(pool, nil)

	// 20 equally relevant documents, only the last 5 carrying lang=fr. A
	// post-hoc filter over a top-5 would return nothing.
	for n := range 20 {
		lang := "en"
		if n >= 15 {
			lang = "fr"
		}
		source := fmt.Sprintf("mem://doc-%02d", n)
		if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": lang}); err != nil {
			t.Fatalf("indexing %s: %+v", source, err)
		}
	}

	results, err := idx.SearchFiltered(ctx, filterTestQuery,
		index.Filter{index.Eq("lang", "fr")}, index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("SearchFiltered: %+v", err)
	}

	if len(results) != 5 {
		t.Fatalf("expected 5 matching results, got %d: %v", len(results), sources(results))
	}
	for _, r := range results {
		if r.Source.String() < "mem://doc-15" {
			t.Errorf("result %s does not satisfy the filter", r.Source)
		}
	}
}

// Text comparison must be byte-per-byte, like the Go evaluator. A database
// collation that ignores punctuation or case would silently disagree — and
// would break date ranges, which are text comparisons on canonical timestamps.
func TestSearchFilteredUsesBinaryCollation(t *testing.T) {
	pool := requirePostgres(t)
	ctx := context.Background()

	idx := NewIndex(pool, nil)

	if err := indexWithMetadata(ctx, idx, "mem://upper", map[string]any{"name": "Eric"}); err != nil {
		t.Fatalf("index: %+v", err)
	}
	if err := indexWithMetadata(ctx, idx, "mem://accent", map[string]any{"name": "Éric"}); err != nil {
		t.Fatalf("index: %+v", err)
	}

	matched := func(filter index.Filter) []string {
		results, err := idx.SearchFiltered(ctx, filterTestQuery, filter, index.SearchOptions{MaxResults: 100})
		if err != nil {
			t.Fatalf("SearchFiltered: %+v", err)
		}
		return sources(results)
	}

	if got := matched(index.Filter{index.Eq("name", "eric")}); len(got) != 0 {
		t.Errorf("equality must be case-sensitive, matched %v", got)
	}
	if got := matched(index.Filter{index.Eq("name", "Eric")}); fmt.Sprint(got) != "[mem://upper]" {
		t.Errorf("Eq(name, Eric) = %v, want [mem://upper]", got)
	}
	if got := matched(index.Filter{index.Eq("name", "Éric")}); fmt.Sprint(got) != "[mem://accent]" {
		t.Errorf("equality must be accent-sensitive, got %v", got)
	}
}

// Re-indexing must refresh the metadata copy and deleting must drop it: a stale
// copy would filter on values this index no longer holds.
func TestDocumentMetadataFollowsTheDocument(t *testing.T) {
	pool := requirePostgres(t)
	ctx := context.Background()

	idx := NewIndex(pool, nil)
	source := "mem://doc"

	matches := func(filter index.Filter) bool {
		results, err := idx.SearchFiltered(ctx, filterTestQuery, filter, index.SearchOptions{MaxResults: 10})
		if err != nil {
			t.Fatalf("SearchFiltered: %+v", err)
		}
		return len(results) > 0
	}

	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "fr"}); err != nil {
		t.Fatalf("index: %+v", err)
	}
	if !matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Fatal("freshly indexed metadata is not filterable")
	}

	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "en"}); err != nil {
		t.Fatalf("reindex: %+v", err)
	}
	if matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Error("stale metadata survived a re-index")
	}
	if !matches(index.Filter{index.Eq("lang", "en")}) {
		t.Error("refreshed metadata is not filterable")
	}

	if err := indexWithMetadata(ctx, idx, source, nil); err != nil {
		t.Fatalf("reindex without metadata: %+v", err)
	}
	if matches(index.Filter{index.Exists("lang")}) {
		t.Error("metadata survived a re-index that carried none")
	}

	if err := indexWithMetadata(ctx, idx, source, map[string]any{"lang": "fr"}); err != nil {
		t.Fatalf("reindex: %+v", err)
	}
	u, _ := url.Parse(source)
	if err := idx.DeleteBySource(ctx, u); err != nil {
		t.Fatalf("delete: %+v", err)
	}
	if matches(index.Filter{index.Eq("lang", "fr")}) {
		t.Error("metadata survived the deletion of its document")
	}
}

// Hostile keys must be rejected before reaching the SQL builder, and hostile
// values must be treated as data.
func TestSearchFilteredResistsInjection(t *testing.T) {
	pool := requirePostgres(t)
	ctx := context.Background()

	idx := NewIndex(pool, nil)

	if err := indexWithMetadata(ctx, idx, "mem://doc", map[string]any{"name": "'; DROP TABLE amoxtli_chunks; --"}); err != nil {
		t.Fatalf("index: %+v", err)
	}

	_, err := idx.SearchFiltered(ctx, filterTestQuery,
		index.Filter{index.Eq("'; DROP TABLE amoxtli_chunks; --", "x")}, index.SearchOptions{MaxResults: 5})
	if !errors.Is(err, index.ErrInvalidFilterKey) {
		t.Fatalf("expected an ErrInvalidFilterKey, got %+v", err)
	}

	// A hostile *value* is legitimate data and must match itself.
	results, err := idx.SearchFiltered(ctx, filterTestQuery,
		index.Filter{index.Eq("name", "'; DROP TABLE amoxtli_chunks; --")}, index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("SearchFiltered with a hostile value: %+v", err)
	}
	if len(results) != 1 {
		t.Errorf("a hostile value must be matched as data, got %v", sources(results))
	}

	// The table must obviously still be there.
	if _, err := idx.Search(ctx, filterTestQuery, index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("index damaged: %+v", err)
	}
}

func requirePostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()

	pool, err := resetDatabase(t, ctx, startPostgresContainer(t, ctx))
	if err != nil {
		t.Fatalf("could not reset database: %+v", err)
	}

	return pool
}

func indexWithMetadata(ctx context.Context, idx *Index, source string, metadata map[string]any) error {
	doc, err := markdown.Parse([]byte(filterTestBody))
	if err != nil {
		return errors.WithStack(err)
	}

	u, err := url.Parse(source)
	if err != nil {
		return errors.WithStack(err)
	}

	doc.SetSource(u)
	doc.SetMetadata(metadata)

	return errors.WithStack(idx.Index(ctx, doc))
}

func sources(results []*index.SearchResult) []string {
	out := make([]string, 0, len(results))
	for _, r := range results {
		out = append(out, r.Source.String())
	}
	return out
}
