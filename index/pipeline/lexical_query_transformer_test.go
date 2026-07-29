package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

type lexicalOnlyTransformer struct {
	suffix string
	calls  *int32
}

func (t lexicalOnlyTransformer) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return query + t.suffix, nil
}

func (t lexicalOnlyTransformer) LexicalOnly() bool { return true }

var _ LexicalQueryTransformer = lexicalOnlyTransformer{}

// contradictoryTransformer claims both markers, which restrict to nothing.
type contradictoryTransformer struct {
	calls *int32
}

func (t contradictoryTransformer) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	if t.calls != nil {
		atomic.AddInt32(t.calls, 1)
	}
	return query + " NEVER", nil
}

func (t contradictoryTransformer) LexicalOnly() bool  { return true }
func (t contradictoryTransformer) SemanticOnly() bool { return true }

// TestLexicalOnlyQueryTransformation is the mirror of
// TestPerIndexQueryTransformation: a lexical-only transformer (e.g. query
// translation) must reach full-text indexes only, leaving the semantic query
// untouched.
func TestLexicalOnlyQueryTransformation(t *testing.T) {
	semantic := &mockIndex{semantic: true}
	lexical := &mockIndex{semantic: false}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
			NewIdentifiedIndex("fts", lexical):  1,
		},
		WithQueryTransformers(lexicalOnlyTransformer{suffix: " chien"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "dog", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if g := lexical.recordedQuery(); g != "dog chien" {
		t.Errorf("lexical index query = %q, want %q", g, "dog chien")
	}
	if g := semantic.recordedQuery(); g != "dog" {
		t.Errorf("semantic index query = %q, want %q", g, "dog")
	}
}

// The two markers must not bleed into each other: each leg gets its own
// variant, neither sees the other's.
func TestLexicalAndSemanticTransformersStayOnTheirOwnLeg(t *testing.T) {
	semantic := &mockIndex{semantic: true}
	lexical := &mockIndex{semantic: false}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
			NewIdentifiedIndex("fts", lexical):  1,
		},
		WithQueryTransformers(
			QueryTransformerFunc(func(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
				return query + " BOTH", nil
			}),
			lexicalOnlyTransformer{suffix: " LEX"},
			semanticOnlyTransformer{suffix: " SEM"},
		),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	// Both legs start from the universal transformation, then diverge.
	if g := lexical.recordedQuery(); g != "base BOTH LEX" {
		t.Errorf("lexical index query = %q, want %q", g, "base BOTH LEX")
	}
	if g := semantic.recordedQuery(); g != "base BOTH SEM" {
		t.Errorf("semantic index query = %q, want %q", g, "base BOTH SEM")
	}
}

// A lexical-only transformer — and the LLM call a translation would make — must
// not run for a pipeline that has no lexical index at all.
func TestLexicalOnlyTransformerSkippedForSemanticPipeline(t *testing.T) {
	var calls int32
	semantic := &mockIndex{semantic: true}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
		},
		WithQueryTransformers(lexicalOnlyTransformer{suffix: " LEX", calls: &calls}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("lexical-only transformer invoked %d time(s) without a lexical index", n)
	}
	if g := semantic.recordedQuery(); g != "base" {
		t.Errorf("semantic index query = %q, want %q", g, "base")
	}
}

// Declaring both markers restricts to nothing; the transformer must be applied
// nowhere rather than to everything (NewIndex warns about it).
func TestContradictoryMarkersApplyNowhere(t *testing.T) {
	var calls int32
	semantic := &mockIndex{semantic: true}
	lexical := &mockIndex{semantic: false}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
			NewIdentifiedIndex("fts", lexical):  1,
		},
		WithQueryTransformers(contradictoryTransformer{calls: &calls}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("contradictory transformer invoked %d time(s), want 0", n)
	}
	if g := lexical.recordedQuery(); g != "base" {
		t.Errorf("lexical index query = %q, want %q", g, "base")
	}
	if g := semantic.recordedQuery(); g != "base" {
		t.Errorf("semantic index query = %q, want %q", g, "base")
	}
}

// The two legs must not serialise on each other's transformation: the semantic
// leg is searched while the lexical transformer is still blocked.
func TestSemanticSearchOverlapsLexicalTransform(t *testing.T) {
	semantic := &mockIndex{semantic: true, searched: make(chan struct{})}
	lexical := &mockIndex{semantic: false}

	transformer := &blockingLexicalTransformer{
		suffix:   " LEX",
		unblock:  semantic.searched,
		deadline: 5 * time.Second,
	}

	idx := NewIndex(
		WeightedIndexes{
			NewIdentifiedIndex("vec", semantic): 1,
			NewIdentifiedIndex("fts", lexical):  1,
		},
		WithQueryTransformers(transformer),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := idx.Search(ctx, "base", index.SearchOptions{MaxResults: 5}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if transformer.timedOut.Load() {
		t.Error("lexical transform did not overlap the semantic search: the legs are serialised")
	}
	if g := lexical.recordedQuery(); g != "base LEX" {
		t.Errorf("lexical index query = %q, want %q", g, "base LEX")
	}
}

type blockingLexicalTransformer struct {
	suffix   string
	unblock  <-chan struct{}
	deadline time.Duration
	timedOut atomic.Bool
}

func (t *blockingLexicalTransformer) TransformQuery(ctx context.Context, query string, opts index.SearchOptions) (string, error) {
	select {
	case <-t.unblock:
	case <-time.After(t.deadline):
		t.timedOut.Store(true)
	}
	return query + t.suffix, nil
}

func (t *blockingLexicalTransformer) LexicalOnly() bool { return true }
