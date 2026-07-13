package amoxtli

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

const testDocument = `# Bœuf bourguignon

Le bœuf bourguignon est une recette de cuisine traditionnelle française.

## Histoire

La recette daterait du Moyen Âge.
`

func TestCodexSmoke(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("https://example.net/boeuf.md")

	taskID, err := codex.IndexFile(ctx, collID, "boeuf.md", strings.NewReader(testDocument),
		WithIndexFileSource(source),
	)
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	// Wait for the async indexing task to complete
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			t.Fatalf("could not get task state: %+v", errors.WithStack(err))
		}

		if state.Status == task.StatusSucceeded {
			break
		}

		if state.Status == task.StatusFailed {
			t.Fatalf("indexing task failed: %+v", state.Error)
		}

		if time.Now().After(deadline) {
			t.Fatalf("indexing task did not finish in time (status: %s)", state.Status)
		}

		time.Sleep(100 * time.Millisecond)
	}

	results, err := codex.Search(ctx, "recette bourguignon", WithSearchMaxResults(5))
	if err != nil {
		t.Fatalf("could not search: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Fatalf("len(results): no results")
	}

	if e, g := source.String(), results[0].Source.String(); e != g {
		t.Errorf("results[0].Source.String(): expected %s, got %s", e, g)
	}

	sections, err := codex.GetSectionsByIDs(ctx, results[0].Sections)
	if err != nil {
		t.Fatalf("could not get sections: %+v", errors.WithStack(err))
	}

	if len(sections) == 0 {
		t.Errorf("len(sections): expected sections, got none")
	}

	// Backup / Restore roundtrip
	snapshot, err := codex.Backup(ctx)
	if err != nil {
		t.Fatalf("could not backup: %+v", errors.WithStack(err))
	}

	if err := codex.Restore(ctx, snapshot); err != nil {
		t.Fatalf("could not restore: %+v", errors.WithStack(err))
	}
	snapshot.Close()

	results, err = codex.Search(ctx, "recette bourguignon")
	if err != nil {
		t.Fatalf("could not search after restore: %+v", errors.WithStack(err))
	}

	if len(results) == 0 {
		t.Fatalf("len(results) after restore: no results")
	}
}

// failingLLM is an llm.Client whose every call fails, used to exercise the
// grounding fail-open / fail-closed behaviour of Search.
type failingLLM struct{ err error }

func (f failingLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return nil, f.err
}

func (f failingLLM) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, f.err
}

func (f failingLLM) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	return nil, f.err
}

// newGroundingCodex builds a bleve-backed Codex with grounding enabled and an
// LLM client that always fails, indexes the test document and returns the ready
// Codex. HyDE never calls the LLM here because bleve is not a semantic index, so
// the only LLM call in Search is the grounding evaluator.
func newGroundingCodex(t *testing.T, failOpen bool) *Codex {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { bleveIdx.Close() })

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { store.Close() })

	options := []Option{
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithLLMClient(failingLLM{err: errors.New("llm unavailable")}),
		WithGroundingCheck(),
	}
	if failOpen {
		options = append(options, WithGroundingFailOpen())
	}

	codex, err := New(ctx, options...)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { codex.Close() })

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("https://example.net/boeuf.md")
	taskID, err := codex.IndexFile(ctx, collID, "boeuf.md", strings.NewReader(testDocument), WithIndexFileSource(source))
	if err != nil {
		t.Fatalf("could not index file: %+v", errors.WithStack(err))
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			t.Fatalf("could not get task state: %+v", errors.WithStack(err))
		}
		if state.Status == task.StatusSucceeded {
			break
		}
		if state.Status == task.StatusFailed {
			t.Fatalf("indexing task failed: %+v", state.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexing task did not finish in time (status: %s)", state.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}

	return codex
}

func TestSearchGroundingFailOpen(t *testing.T) {
	codex := newGroundingCodex(t, true)

	results, err := codex.Search(context.Background(), "recette bourguignon", WithSearchMaxResults(5))
	if err != nil {
		t.Fatalf("fail-open Search should not error when the evaluator fails: %+v", errors.WithStack(err))
	}
	if len(results) == 0 {
		t.Fatal("fail-open Search returned no results (expected the unfiltered results)")
	}
}

func TestSearchGroundingFailClosed(t *testing.T) {
	codex := newGroundingCodex(t, false)

	if _, err := codex.Search(context.Background(), "recette bourguignon", WithSearchMaxResults(5)); err == nil {
		t.Fatal("fail-closed Search should error when the evaluator fails")
	}
}
