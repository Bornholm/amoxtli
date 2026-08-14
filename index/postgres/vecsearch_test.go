package postgres

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// TestVectorLegRaisesEFSearch exercises the KNN path against a real pgvector
// server: the SET LOCAL must be accepted, the transaction must return the hits,
// and the setting must not stay stuck on the connection once it is back in the
// pool.
//
// It uses a stub embeddings client rather than Ollama, so it needs only the
// PostgreSQL container.
func TestVectorLegRaisesEFSearch(t *testing.T) {
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

	idx := NewIndex(pool, &stubEmbedder{dims: DefaultVectorSize}, WithEmbeddingsModel("stub"))

	for n := range 30 {
		document, err := markdown.Parse(fmt.Appendf(nil, "# Title %d\n\nsome searchable content number %d\n", n, n))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}

		source, err := url.Parse(fmt.Sprintf("mem://doc-%d", n))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		document.SetSource(source)

		if err := idx.Index(ctx, document); err != nil {
			t.Fatalf("%+v", err)
		}
	}

	// A limit well above pgvector's default ef_search of 40: this is the case
	// that used to come back capped.
	results, err := idx.Search(ctx, "searchable content", index.SearchOptions{MaxResults: 100})
	if err != nil {
		t.Fatalf("%+v", err)
	}

	if len(results) == 0 {
		t.Fatal("expected the hybrid search to return hits")
	}

	// The setting is transaction-scoped: a later query on a recycled connection
	// must see the server default again, not our raised value.
	var efSearch string
	if err := pool.QueryRow(ctx, `SHOW hnsw.ef_search`).Scan(&efSearch); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	if efSearch != "40" {
		t.Errorf("hnsw.ef_search = %s outside the transaction, want the untouched default 40", efSearch)
	}
}

// stubEmbedder returns deterministic vectors without a model server.
type stubEmbedder struct {
	llm.Client

	dims int
}

func (e *stubEmbedder) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	vecs := make([][]float64, len(inputs))
	for i, input := range inputs {
		vec := make([]float64, e.dims)
		for d := range vec {
			// Deterministic, input-dependent and non-degenerate, which is all the
			// KNN needs to produce a stable ordering.
			vec[d] = float64((len(input)+d)%17) + 1
		}
		vecs[i] = vec
	}

	return stubEmbeddings{vecs: vecs}, nil
}

type stubEmbeddings struct{ vecs [][]float64 }

func (r stubEmbeddings) Embeddings() [][]float64    { return r.vecs }
func (r stubEmbeddings) Usage() llm.EmbeddingsUsage { return llm.NewEmbeddingsUsage(0, 0) }
