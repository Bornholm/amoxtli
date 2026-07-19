package cli

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeLLMServer is an OpenAI-compatible /embeddings + /chat/completions
// endpoint returning deterministic answers, counting the requests it serves so
// tests can assert cache hits never reach the endpoints.
type fakeLLMServer struct {
	*httptest.Server
	requests     atomic.Int64
	chatRequests atomic.Int64
}

const fakeVectorSize = 8

// fakeChatAnswer parses as the judge/evaluator JSON verdict and doubles as a
// plain HyDE answer.
const fakeChatAnswer = `{"identifiers": [], "explanation": "none"}`

func newFakeLLMServer(t *testing.T) *fakeLLMServer {
	t.Helper()

	s := &fakeLLMServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/embeddings"):
			s.requests.Add(1)

			var req struct {
				Input []string `json:"input"`
				Model string   `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}

			type item struct {
				Object    string    `json:"object"`
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			}

			data := make([]item, 0, len(req.Input))
			for i, input := range req.Input {
				data = append(data, item{Object: "embedding", Index: i, Embedding: fakeVector(input)})
			}

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data":   data,
				"model":  req.Model,
				"usage":  map[string]int{"prompt_tokens": 1, "total_tokens": 1},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			s.chatRequests.Add(1)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "cmpl-1",
				"object": "chat.completion",
				"model":  "test-chat",
				"choices": []map[string]any{{
					"index":         0,
					"finish_reason": "stop",
					"message":       map[string]any{"role": "assistant", "content": fakeChatAnswer},
				}},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)

	return s
}

// fakeVector derives a stable, non-zero vector from the input text.
func fakeVector(input string) []float64 {
	sum := sha256.Sum256([]byte(input))
	vec := make([]float64, fakeVectorSize)
	for i := range vec {
		vec[i] = float64(binary.LittleEndian.Uint16(sum[i*2:])) + 1
	}
	return vec
}

// cacheTestDocument deliberately avoids soft line-wraps inside paragraphs: the
// markdown parser collapses them into spaces while the store persists the raw
// text, so a wrapped paragraph is embedded under two different texts on add vs
// reindex (one extra, expected miss). Single-line paragraphs keep the chunk
// text — and thus the cache key — identical on both paths.
const cacheTestDocument = `# Go Programming Language

Go is a statically typed, compiled programming language designed at Google.

## Concurrency

Goroutines and channels form the core of Go's concurrency model.
`

// TestCLIEmbeddingsCache checks that the embeddings cache is wired in by
// default: a reindex of unchanged content and a repeated query are served from
// the cache without reaching the embeddings endpoint, and "cache purge"
// empties it.
func TestCLIEmbeddingsCache(t *testing.T) {
	root := t.TempDir()
	server := newFakeLLMServer(t)

	docPath := filepath.Join(root, "go-intro.md")
	if err := os.WriteFile(docPath, []byte(cacheTestDocument), 0600); err != nil {
		t.Fatal(err)
	}

	configDir := filepath.Join(root, ".amoxtli")
	if err := os.MkdirAll(configDir, 0750); err != nil {
		t.Fatal(err)
	}

	configYAML := fmt.Sprintf(`version: 1
store:
  driver: sqlite
  dsn: data/store.sqlite
index:
  driver: local
  fulltext:
    enabled: true
    path: data/index.bleve
    weight: 1.0
  vector:
    enabled: auto
    path: data/vectors.sqlite
    weight: 1.0
    vector_size: %d
llm:
  chat:
    provider: openai
    base_url: %s
    model: test-chat
    api_key: test
  embeddings:
    provider: openai
    base_url: %s
    model: test-embed
    api_key: test
`, fakeVectorSize, server.URL, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}

	// First indexing: the vectors are not cached yet, the endpoint is hit.
	mustRunCLI(t, "-C", root, "add", docPath)
	if server.requests.Load() == 0 {
		t.Fatal("expected the first indexing to call the embeddings endpoint")
	}

	cacheDir := filepath.Join(configDir, "cache", "embeddings")
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) == 0 {
		t.Fatalf("expected cache entries in %s (err: %v)", cacheDir, err)
	}

	// Reindexing unchanged content must be served entirely from the cache.
	before := server.requests.Load()
	mustRunCLI(t, "-C", root, "reindex")
	if delta := server.requests.Load() - before; delta != 0 {
		t.Errorf("expected no embeddings call on reindex of unchanged content, got %d", delta)
	}

	// A repeated identical query must reuse the cached query vector AND the
	// cached seeded chat completions (HyDE expansion, Judge verdict).
	mustRunCLI(t, "-C", root, "search", "concurrency goroutines")
	if server.chatRequests.Load() == 0 {
		t.Fatal("expected the first search to call the chat endpoint (HyDE/Judge)")
	}
	before = server.requests.Load()
	chatBefore := server.chatRequests.Load()
	mustRunCLI(t, "-C", root, "search", "concurrency goroutines")
	if delta := server.requests.Load() - before; delta != 0 {
		t.Errorf("expected no embeddings call on a repeated query, got %d", delta)
	}
	if delta := server.chatRequests.Load() - chatBefore; delta != 0 {
		t.Errorf("expected no chat call on a repeated query (seeded completions are cached), got %d", delta)
	}

	// cache purge empties the cache directory; the next reindex hits the
	// endpoint again.
	output := mustRunCLI(t, "-C", root, "cache", "purge")
	if !strings.Contains(output, "Purged LLM cache") {
		t.Errorf("unexpected cache purge output: %s", output)
	}
	if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, got err %v", cacheDir, err)
	}

	before = server.requests.Load()
	mustRunCLI(t, "-C", root, "reindex")
	if delta := server.requests.Load() - before; delta == 0 {
		t.Error("expected embeddings calls after a cache purge")
	}
}
