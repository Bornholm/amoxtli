package pipeline

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func TestIndex(t *testing.T) {
	index := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("first", &mockIndex{}): 1,
			NewIdentifiedIndex("second", &mockIndex{
				indexErr: errors.New("Oh snap !"),
			}): 1,
			NewIdentifiedIndex("third", &mockIndex{
				indexErr: errors.New("Oh snap !"),
			}): 1,
		},
	)

	document, err := markdown.Parse([]byte(""))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := index.Index(ctx, document); err == nil {
		t.Error("err should not be nil")
	}
}

func TestSearchRespectsMaxResults(t *testing.T) {
	results := make([]*index.SearchResult, 0, 5)
	for n := 0; n < 5; n++ {
		source, err := url.Parse(fmt.Sprintf("mem://doc-%d", n))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		results = append(results, &index.SearchResult{
			Source:   source,
			Sections: []model.SectionID{model.SectionID(fmt.Sprintf("section-%d", n))},
		})
	}

	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("first", &mockIndex{results: results}): 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const maxResults = 3
	got, err := idx.Search(ctx, "query", index.SearchOptions{MaxResults: maxResults})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if len(got) != maxResults {
		t.Errorf("len(got) = %d, want %d", len(got), maxResults)
	}
}

// TestMergeResultsRRFRanking checks that fusion is rank-sensitive (a top-ranked
// result outweighs a low-ranked one) and rewards cross-index consensus, and
// that the fused scores are exposed and strictly ordered.
func TestMergeResultsRRFRanking(t *testing.T) {
	const (
		docA = "mem://docA"
		docB = "mem://docB"
		docC = "mem://docC"
	)

	// vec ranks A above B; fts ranks B above C. B is the only document
	// corroborated by both legs and must therefore rank first.
	vec := []*index.SearchResult{makeResult(t, docA, "a"), makeResult(t, docB, "b")}
	fts := []*index.SearchResult{makeResult(t, docB, "b"), makeResult(t, docC, "c")}

	idx := NewIndex(WeightedIndexes{
		NewIdentifiedIndex("vec", &mockIndex{results: vec}): 1,
		NewIdentifiedIndex("fts", &mockIndex{results: fts}): 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	got, err := idx.Search(ctx, "query", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	want := []string{docB, docA, docC}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d", len(got), len(want))
	}

	for rank, w := range want {
		if g := got[rank].Source.String(); g != w {
			t.Errorf("got[%d].Source = %s, want %s", rank, g, w)
		}
	}

	// Scores must be exposed and strictly descending with the ranking.
	for rank := range got {
		if got[rank].Score <= 0 {
			t.Errorf("got[%d].Score = %v, want > 0", rank, got[rank].Score)
		}
		if rank > 0 && got[rank-1].Score <= got[rank].Score {
			t.Errorf("scores not strictly descending: got[%d]=%v <= got[%d]=%v", rank-1, got[rank-1].Score, rank, got[rank].Score)
		}
	}
}

// TestPerIndexQueryTransformation checks that a semantic-only query transformer
// (e.g. HyDE) is applied only to semantic indexes, while lexical indexes see the
// untransformed query.
func TestPerIndexQueryTransformation(t *testing.T) {
	semantic := &mockIndex{semantic: true}
	lexical := &mockIndex{semantic: false}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
			NewIdentifiedIndex("fts", lexical):  1,
		},
		WithQueryTransformers(semanticOnlyTransformer{suffix: " HYDE"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if g := semantic.recordedQuery(); g != "base HYDE" {
		t.Errorf("semantic index query = %q, want %q", g, "base HYDE")
	}
	if g := lexical.recordedQuery(); g != "base" {
		t.Errorf("lexical index query = %q, want %q", g, "base")
	}
}

// TestSemanticOnlyTransformerSkippedForLexicalPipeline checks that a
// semantic-only transformer (and its potential LLM call) is never invoked when
// no registered index is semantic.
func TestSemanticOnlyTransformerSkippedForLexicalPipeline(t *testing.T) {
	var calls int32
	lexical := &mockIndex{semantic: false}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("fts", lexical): 1,
		},
		WithQueryTransformers(semanticOnlyTransformer{suffix: " HYDE", calls: &calls}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("semantic-only transformer invoked %d time(s) without a semantic index", n)
	}
	if g := lexical.recordedQuery(); g != "base" {
		t.Errorf("lexical index query = %q, want %q", g, "base")
	}
}

func makeResult(t *testing.T, rawURL string, section string) *index.SearchResult {
	t.Helper()
	source, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	return &index.SearchResult{
		Source:   source,
		Sections: []model.SectionID{model.SectionID(section)},
	}
}

// semanticOnlyTransformer appends suffix to the query and declares itself
// semantic-only; it optionally counts invocations.
type semanticOnlyTransformer struct {
	suffix string
	calls  *int32
}

func (t semanticOnlyTransformer) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return query + t.suffix, nil
}

func (t semanticOnlyTransformer) SemanticOnly() bool { return true }

var _ SemanticQueryTransformer = semanticOnlyTransformer{}

type mockIndex struct {
	indexErr error
	results  []*index.SearchResult
	semantic bool

	mu        sync.Mutex
	lastQuery string
}

// Semantic implements index.Semantic.
func (m *mockIndex) Semantic() bool { return m.semantic }

// All implements index.Index.
func (m *mockIndex) All(ctx context.Context, yield func(model.SectionID) bool) error {
	panic("unimplemented")
}

// DeleteByID implements index.Index.
func (m *mockIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	panic("unimplemented")
}

// DeleteBySource implements index.Index.
func (m *mockIndex) DeleteBySource(ctx context.Context, source *url.URL) error {
	return nil
}

// Index implements index.Index.
func (m *mockIndex) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	return m.indexErr
}

// Search implements index.Index.
func (m *mockIndex) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	m.mu.Lock()
	m.lastQuery = query
	m.mu.Unlock()
	return m.results, nil
}

func (m *mockIndex) recordedQuery() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastQuery
}

var _ index.Index = &mockIndex{}
