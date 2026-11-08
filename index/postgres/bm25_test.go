package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/testsuite"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

// TestIndexFullTextOnlyBM25 runs the conformance suite against the BM25
// full-text leg alone. Its documents are a handful of chunks: every lexeme is
// in more than a tenth of them, which the cutoff's floor must keep.
func TestIndexFullTextOnlyBM25(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()
	connectionStr := startPostgresContainer(t, ctx)

	testsuite.TestIndex(t, func(t *testing.T) (index.Index, error) {
		pool, err := resetDatabase(t, ctx, connectionStr)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return NewIndex(pool, nil, WithBM25(true)), nil
	})
}

// TestBM25StatisticsFollowRestore checks that a restore, which replaces the
// whole table, does not leave the search on the statistics of the chunks it
// removed.
func TestBM25StatisticsFollowRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()
	connectionStr := startPostgresContainer(t, ctx)
	pool, err := resetDatabase(t, ctx, connectionStr)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	idx := NewIndex(pool, nil, WithBM25(true))
	indexDocs(t, ctx, idx, "lighthouse", 3)

	snapshot, err := idx.GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	data, err := io.ReadAll(snapshot)
	snapshot.Close()
	if err != nil {
		t.Fatalf("%+v", err)
	}

	// More chunks, and statistics computed on them.
	indexDocs(t, ctx, idx, "harbour", 12)
	if _, err := idx.Search(ctx, "harbour", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", err)
	}
	if got := statsChunks(t, ctx, pool); got != 15 {
		t.Fatalf("statistics on %d chunks before the restore, want 15", got)
	}

	if err := idx.RestoreSnapshot(ctx, bytes.NewReader(data)); err != nil {
		t.Fatalf("%+v", err)
	}
	results, err := idx.Search(ctx, "lighthouse", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("%+v", err)
	}
	if len(results) != 3 {
		t.Errorf("%d results after the restore, want 3", len(results))
	}
	if got := statsChunks(t, ctx, pool); got != 3 {
		t.Errorf("statistics on %d chunks after the restore, want 3", got)
	}
}

func indexDocs(t *testing.T, ctx context.Context, idx *Index, word string, n int) {
	t.Helper()
	for k := range n {
		document, err := markdown.Parse(fmt.Appendf(nil, "# %s %d\n\nthe %s number %d\n", word, k, word, k))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		source, err := url.Parse(fmt.Sprintf("mem://%s-%d", word, k))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		document.SetSource(source)
		if err := idx.Index(ctx, document); err != nil {
			t.Fatalf("%+v", err)
		}
	}
}

func statsChunks(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var chunks int64
	if err := pool.QueryRow(ctx, `SELECT coalesce((SELECT chunks FROM amoxtli_lexeme_stats_meta WHERE id = 1), 0)`).Scan(&chunks); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	return chunks
}
