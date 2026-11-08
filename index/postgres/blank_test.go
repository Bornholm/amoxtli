package postgres

import (
	"testing"

	"github.com/bornholm/amoxtli/markdown"
)

// TestCollectChunksSkipsBlankSections: markdown.Trim renders an empty chunk as
// "\n", so checking the length alone stored a chunk without text and sent it to
// the embedding model.
func TestCollectChunksSkipsBlankSections(t *testing.T) {
	// The frontmatter holds the whole file: the root section is left with an
	// empty range, and its content is "\n".
	document, err := markdown.Parse([]byte("---\nsource: file:///test/blank.md\n---\n"))
	if err != nil {
		t.Fatalf("markdown.Parse() error: %+v", err)
	}

	chunks, err := collectChunks(document, 1000)
	if err != nil {
		t.Fatalf("collectChunks() error: %+v", err)
	}

	if len(chunks) != 0 {
		t.Errorf("collected %d chunks, expected the blank section to be skipped", len(chunks))
	}
}
