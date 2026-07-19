package llmx

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"
)

func TestCachingClientServesHitsWithoutInnerCalls(t *testing.T) {
	e := &embedderStub{}
	c := newTestCache(t, e)

	first, err := c.Embeddings(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	second, err := c.Embeddings(context.Background(), []string{"alpha", "beta"})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := e.calls(); got != 1 {
		t.Errorf("inner calls = %d, want 1 (second call must be a full cache hit)", got)
	}
	if !reflect.DeepEqual(first.Embeddings(), second.Embeddings()) {
		t.Errorf("cached vectors differ from fetched ones: %v vs %v", second.Embeddings(), first.Embeddings())
	}
	if hits, misses := c.Stats(); hits != 2 || misses != 2 {
		t.Errorf("stats = %d hits / %d misses, want 2 / 2", hits, misses)
	}
	if usage := second.Usage(); usage.TotalTokens() != 0 {
		t.Errorf("full-hit usage = %d total tokens, want 0 (nothing billed)", usage.TotalTokens())
	}
}

func TestCachingClientFetchesOnlyMisses(t *testing.T) {
	e := &embedderStub{}
	c := newTestCache(t, e)

	if _, err := c.Embeddings(context.Background(), []string{"alpha"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	res, err := c.Embeddings(context.Background(), []string{"alpha", "beta", "gamma"})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if want := [][]string{{"alpha"}, {"beta", "gamma"}}; !reflect.DeepEqual(e.batches(), want) {
		t.Errorf("inner batches = %v, want %v (only misses forwarded)", e.batches(), want)
	}

	// The response must be assembled in input order, hits and misses interleaved.
	want := [][]float64{vectorFor("alpha"), vectorFor("beta"), vectorFor("gamma")}
	if !reflect.DeepEqual(res.Embeddings(), want) {
		t.Errorf("embeddings = %v, want %v", res.Embeddings(), want)
	}
}

func TestCachingClientTreatsCorruptedEntryAsMiss(t *testing.T) {
	e := &embedderStub{}
	dir := t.TempDir()
	c, err := NewCachingClient(e, dir, "stub-model")
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if _, err := c.Embeddings(context.Background(), []string{"alpha"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	// Corrupt every cache entry on disk.
	entries, err := filepath.Glob(filepath.Join(dir, "embeddings", "*", "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 cache entry, got %v (err %v)", entries, err)
	}
	if err := os.WriteFile(entries[0], []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt entry: %v", err)
	}

	res, err := c.Embeddings(context.Background(), []string{"alpha"})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := e.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (corrupted entry must be refetched)", got)
	}
	if want := [][]float64{vectorFor("alpha")}; !reflect.DeepEqual(res.Embeddings(), want) {
		t.Errorf("embeddings = %v, want %v", res.Embeddings(), want)
	}

	// The refetch must have repaired the entry: a third call is a pure hit.
	if _, err := c.Embeddings(context.Background(), []string{"alpha"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := e.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (entry must be rewritten after corruption)", got)
	}
}

func TestCachingClientKeysOnDimensions(t *testing.T) {
	e := &embedderStub{}
	c := newTestCache(t, e)

	if _, err := c.Embeddings(context.Background(), []string{"alpha"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if _, err := c.Embeddings(context.Background(), []string{"alpha"}, llm.WithDimensions(64)); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := e.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (different dimensions must not share entries)", got)
	}
}

func newTestCache(t *testing.T, inner llm.Client) *CachingClient {
	t.Helper()
	c, err := NewCachingClient(inner, t.TempDir(), "stub-model")
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	return c
}

// vectorFor is the deterministic embedding the stub returns for an input.
func vectorFor(input string) []float64 {
	return []float64{float64(len(input)), float64(input[0])}
}

// embedderStub records every Embeddings batch and returns deterministic vectors.
type embedderStub struct {
	mu      sync.Mutex
	batched [][]string
}

func (e *embedderStub) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.batched)
}

func (e *embedderStub) batches() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.batched
}

func (e *embedderStub) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	e.mu.Lock()
	e.batched = append(e.batched, append([]string(nil), inputs...))
	e.mu.Unlock()

	vecs := make([][]float64, len(inputs))
	for i, input := range inputs {
		vecs[i] = vectorFor(input)
	}
	return stubEmbeddingsResponse{vecs: vecs}, nil
}

func (e *embedderStub) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "ok"), nil), nil
}

func (e *embedderStub) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	ch := make(chan llm.StreamChunk)
	close(ch)
	return ch, nil
}

type stubEmbeddingsResponse struct{ vecs [][]float64 }

func (r stubEmbeddingsResponse) Embeddings() [][]float64    { return r.vecs }
func (r stubEmbeddingsResponse) Usage() llm.EmbeddingsUsage { return llm.NewEmbeddingsUsage(3, 3) }

var _ llm.Client = &embedderStub{}
