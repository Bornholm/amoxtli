package amoxtli

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/model"
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

// newBleveCodex builds a docker-free bleve + gorm-SQLite Codex and a collection,
// used by the metadata-filtering / pagination end-to-end tests.
func newBleveCodex(t *testing.T) (*Codex, model.CollectionID) {
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

	codex, err := New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithDisableHyDE(),
		WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { codex.Close() })

	collID, err := codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	return codex, collID
}

// indexRecipeAndWait indexes a small document (sharing the term "recette" so
// they are all retrieved by bleve) with the given metadata and blocks until the
// async indexing task completes.
func indexRecipeAndWait(t *testing.T, codex *Codex, collID model.CollectionID, name, rawSource, body string, metadata map[string]any) {
	t.Helper()

	ctx := context.Background()
	source, err := url.Parse(rawSource)
	if err != nil {
		t.Fatalf("bad source %q: %+v", rawSource, err)
	}

	taskID, err := codex.IndexFile(ctx, collID, name, strings.NewReader(body),
		WithIndexFileSource(source),
		WithIndexFileMetadata(metadata),
	)
	if err != nil {
		t.Fatalf("could not index file %q: %+v", name, errors.WithStack(err))
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			t.Fatalf("could not get task state: %+v", errors.WithStack(err))
		}
		if state.Status == task.StatusSucceeded {
			return
		}
		if state.Status == task.StatusFailed {
			t.Fatalf("indexing task for %q failed: %+v", name, state.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexing task for %q did not finish in time (status: %s)", name, state.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// hostsOf returns the set of result source hosts, used to assert which
// documents survived filtering.
func hostsOf(results []*index.SearchResult) map[string]bool {
	hosts := map[string]bool{}
	for _, r := range results {
		hosts[r.Source.Host] = true
	}
	return hosts
}

// TestCodexMetadataFiltering exercises the full ingestion + metadata-filtering
// path end-to-end: three documents are indexed with distinct metadata, then
// searched with metadata filters that must narrow the results accordingly.
func TestCodexMetadataFiltering(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	indexRecipeAndWait(t, codex, collID, "boeuf.md", "https://boeuf/r.md",
		"# Bœuf\n\nUne recette traditionnelle de bœuf bourguignon.",
		map[string]any{"cuisine": "française", "year": 1900})
	indexRecipeAndWait(t, codex, collID, "cassoulet.md", "https://cassoulet/r.md",
		"# Cassoulet\n\nUne recette traditionnelle de cassoulet.",
		map[string]any{"cuisine": "française", "year": 2010})
	indexRecipeAndWait(t, codex, collID, "pasta.md", "https://pasta/r.md",
		"# Pasta\n\nUne recette traditionnelle de pâtes.",
		map[string]any{"cuisine": "italienne", "year": 2020})

	// No filter: all three recipes are retrieved.
	all, err := codex.Search(ctx, "recette traditionnelle", WithSearchMaxResults(10))
	if err != nil {
		t.Fatalf("unfiltered search: %+v", errors.WithStack(err))
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 unfiltered results, got %d (%v)", len(all), hostsOf(all))
	}

	// Equality filter on a string metadata value.
	french, err := codex.Search(ctx, "recette traditionnelle",
		WithSearchMaxResults(10),
		WithSearchFilter(index.Eq("cuisine", "française")),
	)
	if err != nil {
		t.Fatalf("filtered search: %+v", errors.WithStack(err))
	}
	hosts := hostsOf(french)
	if len(french) != 2 || !hosts["boeuf"] || !hosts["cassoulet"] {
		t.Fatalf("expected boeuf+cassoulet for cuisine=française, got %v", hosts)
	}

	// Range filter on a numeric metadata value (stored via JSON as float64).
	recent, err := codex.Search(ctx, "recette traditionnelle",
		WithSearchMaxResults(10),
		WithSearchFilter(index.Gte("year", 2015)),
	)
	if err != nil {
		t.Fatalf("range search: %+v", errors.WithStack(err))
	}
	hosts = hostsOf(recent)
	if len(recent) != 1 || !hosts["pasta"] {
		t.Fatalf("expected only pasta for year>=2015, got %v", hosts)
	}

	// Membership filter combined via conjunction with a range.
	combined, err := codex.Search(ctx, "recette traditionnelle",
		WithSearchMaxResults(10),
		WithSearchFilter(index.In("cuisine", "française", "espagnole"), index.Lt("year", 2000)),
	)
	if err != nil {
		t.Fatalf("combined search: %+v", errors.WithStack(err))
	}
	hosts = hostsOf(combined)
	if len(combined) != 1 || !hosts["boeuf"] {
		t.Fatalf("expected only boeuf for cuisine∈{fr,es} AND year<2000, got %v", hosts)
	}
}

// TestCodexCursorPagination walks the whole result set one page at a time via
// the opaque cursor and asserts every document is visited exactly once.
func TestCodexCursorPagination(t *testing.T) {
	ctx := context.Background()
	codex, collID := newBleveCodex(t)

	// Distinct term frequencies give each document a distinct, stable relevance
	// score (identical content would tie, and bleve orders ties nondetermin-
	// istically, which is the documented limit of cursor pagination).
	for _, r := range []struct{ name, src, body string }{
		{"boeuf.md", "https://boeuf/r.md", "# Bœuf\n\nUne recette recette recette traditionnelle de bœuf."},
		{"cassoulet.md", "https://cassoulet/r.md", "# Cassoulet\n\nUne recette recette traditionnelle de cassoulet."},
		{"pasta.md", "https://pasta/r.md", "# Pasta\n\nUne recette traditionnelle de pâtes."},
	} {
		indexRecipeAndWait(t, codex, collID, r.name, r.src, r.body,
			map[string]any{"kind": "recipe"})
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		page, err := codex.SearchPage(ctx, "recette traditionnelle",
			WithSearchMaxResults(1),
			WithSearchCursor(cursor),
		)
		if err != nil {
			t.Fatalf("page search: %+v", errors.WithStack(err))
		}
		if len(page.Results) == 0 {
			break
		}
		for _, r := range page.Results {
			if seen[r.Source.Host] {
				t.Fatalf("document %q returned twice across pages", r.Source.Host)
			}
			seen[r.Source.Host] = true
		}
		pages++
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 3 || !seen["boeuf"] || !seen["cassoulet"] || !seen["pasta"] {
		t.Fatalf("expected all 3 documents across pages, got %v", seen)
	}
}
