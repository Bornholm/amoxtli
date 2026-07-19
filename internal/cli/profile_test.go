package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestCLIRetrievalProfileFast checks the "fast" retrieval profile: even with a
// chat client configured, a search performs no chat call at all (no HyDE, no
// Judge) — only the query embedding.
func TestCLIRetrievalProfileFast(t *testing.T) {
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
retrieval:
  profile: fast
`, fakeVectorSize, server.URL, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}

	mustRunCLI(t, "-C", root, "add", docPath)

	embeddingsBefore := server.requests.Load()
	mustRunCLI(t, "-C", root, "search", "concurrency goroutines")

	if got := server.chatRequests.Load(); got != 0 {
		t.Errorf("fast profile: expected 0 chat calls, got %d", got)
	}
	if delta := server.requests.Load() - embeddingsBefore; delta != 1 {
		t.Errorf("fast profile: expected exactly 1 embeddings call for the query, got %d", delta)
	}
}
