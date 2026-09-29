package sqlitevec

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/genai/llm"
)

// legislativeMarkdown renders a code chapter holding every article in a single
// section, article titles being bold paragraphs rather than headings — the
// shape pandoc gives most legislative texts.
func legislativeMarkdown(articles []string) string {
	return "# Code de test\n\n## Chapitre Ier\n\n" + strings.Join(articles, "\n\n") + "\n"
}

func legislativeArticle(r *rand.Rand, n int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Article L. %d**", n)
	for range 1 + r.Intn(3) {
		sb.WriteString("\n\n")
		for w := range 20 + r.Intn(100) {
			if w > 0 {
				sb.WriteByte(' ')
			}
			fmt.Fprintf(&sb, "mot%d", r.Intn(5000))
		}
		sb.WriteByte('.')
	}
	return sb.String()
}

// TestReindexReusesUnchangedEmbeddings checks that re-indexing a long section
// after a local edit only sends the chunks around the edit to the embeddings
// model: sections and oversized-section chunks are cut at content-defined
// boundaries, so the others keep their exact text and hit the cache.
func TestReindexReusesUnchangedEmbeddings(t *testing.T) {
	ctx := context.Background()

	r := rand.New(rand.NewSource(1))
	var articles []string
	for i := range 60 {
		articles = append(articles, legislativeArticle(r, i+1))
	}

	cache, err := llmx.NewCachingClient(&keywordClient{}, t.TempDir(), "mock")
	if err != nil {
		t.Fatalf("%+v", err)
	}

	idx := newHermeticIndex(t, cache)

	indexDoc(t, ctx, idx, "mem://code", legislativeMarkdown(articles))
	_, firstMisses := cache.Stats()

	// Insert a new article at the top of the chapter.
	edited := append([]string{legislativeArticle(r, 0)}, articles...)
	indexDoc(t, ctx, idx, "mem://code", legislativeMarkdown(edited))

	hits, misses := cache.Stats()
	misses -= firstMisses
	total := hits + misses

	t.Logf("first indexation: %d chunks; re-indexation: %d of %d chunks embedded again", firstMisses, misses, total)

	if share := float64(misses) / float64(total); share > 0.15 {
		t.Errorf("%d of %d chunks embedded again (%.0f%%), want only the chunks around the edit", misses, total, share*100)
	}
}

// recordingClient records every text sent to the embeddings model.
type recordingClient struct {
	keywordClient
	mu     sync.Mutex
	inputs []string
}

func (c *recordingClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	c.mu.Lock()
	c.inputs = append(c.inputs, inputs...)
	c.mu.Unlock()
	return c.keywordClient.Embeddings(ctx, inputs, funcs...)
}

// TestIndexEmbedsIdenticalChunksOnce checks that the chunks shared by a
// section and its ancestors — whose content holds theirs — reach the
// embeddings model once, even without a cache.
func TestIndexEmbedsIdenticalChunksOnce(t *testing.T) {
	ctx := context.Background()

	r := rand.New(rand.NewSource(1))
	var articles []string
	for i := range 60 {
		articles = append(articles, legislativeArticle(r, i+1))
	}

	client := &recordingClient{}
	idx := newHermeticIndex(t, client)

	indexDoc(t, ctx, idx, "mem://code", legislativeMarkdown(articles))

	seen := map[string]bool{}
	for _, input := range client.inputs {
		if seen[input] {
			t.Errorf("chunk embedded twice: %.60q", input)
		}
		seen[input] = true
	}
}
