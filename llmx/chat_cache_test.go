package llmx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bornholm/genai/llm"
)

// chatStub is an llm.Client whose ChatCompletion returns a canned answer and
// counts its calls; the embedded interface covers the unused methods.
type chatStub struct {
	llm.Client

	mu    sync.Mutex
	count int
}

func (s *chatStub) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	s.mu.Lock()
	s.count++
	n := s.count
	s.mu.Unlock()
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, fmt.Sprintf("answer #%d", n)),
		llm.NewChatCompletionUsage(10, 5, 15),
	), nil
}

func (s *chatStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func newChatTestCache(t *testing.T, inner llm.Client) *CachingClient {
	t.Helper()
	c, err := NewCachingClient(inner, t.TempDir(), "stub-embed", WithChatCache("stub-chat"))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	return c
}

func seeded(seed int) []llm.ChatCompletionOptionFunc {
	return []llm.ChatCompletionOptionFunc{
		llm.WithMessages(llm.NewMessage(llm.RoleUser, "hypothetical answer please")),
		llm.WithTemperature(0.2),
		llm.WithSeed(seed),
	}
}

// TestChatCacheServesSeededCompletionFromCache checks the HyDE-shaped path: an
// identical seeded call is served from the cache with zero billed usage.
func TestChatCacheServesSeededCompletionFromCache(t *testing.T) {
	inner := &chatStub{}
	c := newChatTestCache(t, inner)

	first, err := c.ChatCompletion(context.Background(), seeded(42)...)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	second, err := c.ChatCompletion(context.Background(), seeded(42)...)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := inner.calls(); got != 1 {
		t.Errorf("inner calls = %d, want 1 (second call must be a cache hit)", got)
	}
	if first.Message().Content() != second.Message().Content() {
		t.Errorf("cached content %q differs from fetched %q", second.Message().Content(), first.Message().Content())
	}
	if usage := second.Usage(); usage.TotalTokens() != 0 {
		t.Errorf("cache-hit usage = %d total tokens, want 0 (nothing billed)", usage.TotalTokens())
	}
	if hits, misses := c.ChatStats(); hits != 1 || misses != 1 {
		t.Errorf("chat stats = %d hits / %d misses, want 1 / 1", hits, misses)
	}
}

// TestChatCacheKeyCoversSeedAndMessages checks that a different seed or a
// different prompt is a different cache entry.
func TestChatCacheKeyCoversSeedAndMessages(t *testing.T) {
	inner := &chatStub{}
	c := newChatTestCache(t, inner)

	if _, err := c.ChatCompletion(context.Background(), seeded(1)...); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if _, err := c.ChatCompletion(context.Background(), seeded(2)...); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if _, err := c.ChatCompletion(context.Background(),
		llm.WithMessages(llm.NewMessage(llm.RoleUser, "another prompt")),
		llm.WithSeed(1),
	); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := inner.calls(); got != 3 {
		t.Errorf("inner calls = %d, want 3 (seed and messages must be part of the key)", got)
	}
}

// TestChatCacheSkipsUnseededCalls checks that a call without a seed always
// passes through and writes nothing.
func TestChatCacheSkipsUnseededCalls(t *testing.T) {
	inner := &chatStub{}
	dir := t.TempDir()
	c, err := NewCachingClient(inner, dir, "stub-embed", WithChatCache("stub-chat"))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	for range 2 {
		if _, err := c.ChatCompletion(context.Background(),
			llm.WithMessages(llm.NewMessage(llm.RoleUser, "not deterministic")),
		); err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}
	}

	if got := inner.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (unseeded calls must never be cached)", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "chat", "*", "*.json")); len(entries) != 0 {
		t.Errorf("expected no chat cache entry for unseeded calls, got %v", entries)
	}
	if hits, misses := c.ChatStats(); hits != 0 || misses != 0 {
		t.Errorf("chat stats = %d hits / %d misses, want 0 / 0 (pass-through)", hits, misses)
	}
}

// TestChatCacheDisabledByDefault checks that without WithChatCache every call
// is delegated and nothing is written.
func TestChatCacheDisabledByDefault(t *testing.T) {
	inner := &chatStub{}
	dir := t.TempDir()
	c, err := NewCachingClient(inner, dir, "stub-embed")
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	for range 2 {
		if _, err := c.ChatCompletion(context.Background(), seeded(7)...); err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}
	}

	if got := inner.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (chat cache is opt-in)", got)
	}
	if entries, _ := filepath.Glob(filepath.Join(dir, "chat", "*", "*.json")); len(entries) != 0 {
		t.Errorf("expected no chat cache entry, got %v", entries)
	}
}

// TestChatCacheTreatsCorruptedEntryAsMiss checks the self-healing behavior
// shared with the embeddings cache.
func TestChatCacheTreatsCorruptedEntryAsMiss(t *testing.T) {
	inner := &chatStub{}
	dir := t.TempDir()
	c, err := NewCachingClient(inner, dir, "stub-embed", WithChatCache("stub-chat"))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if _, err := c.ChatCompletion(context.Background(), seeded(9)...); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "chat", "*", "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 chat cache entry, got %v (err %v)", entries, err)
	}
	if err := os.WriteFile(entries[0], []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt entry: %v", err)
	}

	if _, err := c.ChatCompletion(context.Background(), seeded(9)...); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := inner.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (corrupted entry must be refetched)", got)
	}

	// The refetch must have repaired the entry.
	if _, err := c.ChatCompletion(context.Background(), seeded(9)...); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := inner.calls(); got != 2 {
		t.Errorf("inner calls = %d, want 2 (entry must be rewritten after corruption)", got)
	}
}
