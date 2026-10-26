package bleve

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
)

// TestIndexSkipsBlankSections: markdown.Trim renders an empty chunk as "\n", so
// a section without text does not come with an empty content. Checking the
// length alone let bleve index it, while sqlitevec already trimmed it first.
// The skipped section never reports its progress, which must still reach 1.
func TestIndexSkipsBlankSections(t *testing.T) {
	// The frontmatter holds the whole file: the root section is left with an
	// empty range, and its content is "\n".
	document, err := markdown.Parse([]byte("---\nsource: file:///test/blank.md\n---\n"))
	if err != nil {
		t.Fatalf("markdown.Parse() error: %+v", err)
	}

	underlying, err := bleve.New(filepath.Join(t.TempDir(), "index.bleve"), IndexMapping())
	if err != nil {
		t.Fatalf("bleve.New() error: %+v", err)
	}
	t.Cleanup(func() { _ = underlying.Close() })

	var progress []float32

	if err := NewIndex(underlying).Index(context.Background(), document, index.WithOnProgress(func(p float32) {
		progress = append(progress, p)
	})); err != nil {
		t.Fatalf("Index() error: %+v", err)
	}

	count, err := underlying.DocCount()
	if err != nil {
		t.Fatalf("DocCount() error: %+v", err)
	}

	if count != 0 {
		t.Errorf("indexed %d entries, expected the blank section to be skipped", count)
	}

	if len(progress) == 0 || progress[len(progress)-1] != 1 {
		t.Errorf("progress reported: %v, expected it to end at 1", progress)
	}
}
