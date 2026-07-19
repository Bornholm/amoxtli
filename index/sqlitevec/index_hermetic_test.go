package sqlitevec

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/genai/llm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// keywordEmbeddings maps a few control keywords to basis vectors so that a query
// containing a keyword matches (cosine distance ~0) the chunk containing the
// same keyword. It lets us exercise the real Index/Search code paths — including
// the vec0 KNN — without a live embeddings model.
func keywordEmbeddings(text string) []float64 {
	v := []float64{0.01, 0.01, 0.01, 0.01}
	switch {
	case strings.Contains(text, "ALPHA"):
		v[0] = 1
	case strings.Contains(text, "BETA"):
		v[1] = 1
	case strings.Contains(text, "GAMMA"):
		v[2] = 1
	}
	return v
}

// keywordClient is a deterministic llm.Client. Only Embeddings is used by the
// sqlite-vec index; it can be switched to fail to exercise error handling and
// counts its calls so tests can assert how often the endpoint is reached.
type keywordClient struct {
	fail  atomic.Bool
	calls atomic.Int64
}

func (c *keywordClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return nil, errors.New("embeddings unavailable")
	}
	out := make([][]float64, len(inputs))
	for i, in := range inputs {
		out[i] = keywordEmbeddings(in)
	}
	return keywordEmbeddingsResponse{out}, nil
}

func (c *keywordClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *keywordClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

type keywordEmbeddingsResponse struct{ vectors [][]float64 }

func (r keywordEmbeddingsResponse) Embeddings() [][]float64    { return r.vectors }
func (r keywordEmbeddingsResponse) Usage() llm.EmbeddingsUsage { return nil }

var _ llm.Client = &keywordClient{}

func newHermeticIndex(t *testing.T, client llm.Client) *Index {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")
	conn, err := sqlite3.Open(dbFile)
	if err != nil {
		t.Fatalf("failed to open database: %+v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return NewIndex(conn, client,
		WithEmbeddingsModel("mock"),
		WithVectorSize(4),
		WithMaxWords(500),
	)
}

func indexDoc(t *testing.T, ctx context.Context, idx *Index, source, body string) {
	t.Helper()
	doc, err := markdown.Parse([]byte(body))
	if err != nil {
		t.Fatalf("could not parse document: %+v", err)
	}
	u, err := url.Parse(source)
	if err != nil {
		t.Fatalf("could not parse source: %+v", err)
	}
	doc.SetSource(u)
	if err := idx.Index(ctx, doc); err != nil {
		t.Fatalf("could not index %s: %+v", source, errors.WithStack(err))
	}
}

// TestIndexSearchHermetic exercises the full Index -> Search roundtrip (chunk
// collection, out-of-lock embedding, vec0 insert and KNN query) without a live
// LLM, and checks that scores are exposed.
func TestIndexSearchHermetic(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")
	indexDoc(t, ctx, idx, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n")

	results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if got := results[0].Source.String(); got != "mem://alpha" {
		t.Errorf("results[0].Source = %s, want mem://alpha", got)
	}
	if results[0].Score <= 0 {
		t.Errorf("results[0].Score = %v, want > 0", results[0].Score)
	}
}

// TestSearchEmbedsQueryOnce pins the single-embedding property of Search: the
// query is embedded exactly once per call, outside the SQL retry loop, and an
// embeddings failure surfaces immediately without touching the database.
func TestSearchEmbedsQueryOnce(t *testing.T) {
	ctx := context.Background()
	client := &keywordClient{}
	idx := newHermeticIndex(t, client)

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")

	client.calls.Store(0)
	if _, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}
	if got := client.calls.Load(); got != 1 {
		t.Errorf("Search performed %d embeddings calls, want exactly 1", got)
	}

	client.fail.Store(true)
	client.calls.Store(0)
	if _, err := idx.Search(ctx, "another question", index.SearchOptions{MaxResults: 5}); err == nil {
		t.Fatal("expected Search to fail when embeddings are unavailable")
	}
	if got := client.calls.Load(); got != 1 {
		t.Errorf("failed Search performed %d embeddings calls, want exactly 1 (no retry of the network call)", got)
	}
}

// TestIndexFailedEmbeddingsPreservesContent checks the out-of-lock refactor's
// side benefit: when embeddings computation fails during a re-index, the delete
// never runs, so the previously indexed content stays searchable.
func TestIndexFailedEmbeddingsPreservesContent(t *testing.T) {
	ctx := context.Background()
	client := &keywordClient{}
	idx := newHermeticIndex(t, client)

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")

	// Re-index the same source with a failing embeddings client.
	client.fail.Store(true)
	doc, err := markdown.Parse([]byte("# Alpha\n\nUpdated ALPHA content.\n"))
	if err != nil {
		t.Fatalf("parse: %+v", err)
	}
	u, _ := url.Parse("mem://alpha")
	doc.SetSource(u)
	if err := idx.Index(ctx, doc); err == nil {
		t.Fatal("expected Index to fail when embeddings are unavailable")
	}

	// The original content must still be searchable (nothing was deleted).
	client.fail.Store(false)
	results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}
	if len(results) == 0 {
		t.Fatal("previous content was lost after a failed re-index")
	}
	if got := results[0].Source.String(); got != "mem://alpha" {
		t.Errorf("results[0].Source = %s, want mem://alpha", got)
	}
}
