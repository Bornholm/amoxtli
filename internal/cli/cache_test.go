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

// fakeEmbeddingsServer is an OpenAI-compatible /embeddings endpoint returning
// deterministic vectors (derived from the input text), counting the requests
// it serves so tests can assert cache hits never reach the endpoint.
type fakeEmbeddingsServer struct {
	*httptest.Server
	requests atomic.Int64
}

const fakeVectorSize = 8

func newFakeEmbeddingsServer(t *testing.T) *fakeEmbeddingsServer {
	t.Helper()

	s := &fakeEmbeddingsServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/embeddings") {
			http.NotFound(w, r)
			return
		}

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
	server := newFakeEmbeddingsServer(t)

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
  embeddings:
    provider: openai
    base_url: %s
    model: test-embed
    api_key: test
`, fakeVectorSize, server.URL)
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

	// A repeated identical query must reuse the cached query vector.
	mustRunCLI(t, "-C", root, "search", "concurrency goroutines")
	before = server.requests.Load()
	mustRunCLI(t, "-C", root, "search", "concurrency goroutines")
	if delta := server.requests.Load() - before; delta != 0 {
		t.Errorf("expected no embeddings call on a repeated query, got %d", delta)
	}

	// cache purge empties the cache directory; the next reindex hits the
	// endpoint again.
	output := mustRunCLI(t, "-C", root, "cache", "purge")
	if !strings.Contains(output, "Purged embeddings cache") {
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
