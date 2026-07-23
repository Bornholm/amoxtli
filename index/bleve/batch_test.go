package bleve

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/blevesearch/bleve/v2"
	"github.com/bornholm/amoxtli/markdown"
)

// bleveIndex aliases the interface so the embedded field is named "bleveIndex"
// rather than "Index", leaving the name free for the counting method below.
type bleveIndex = bleve.Index

// countingIndex records how many write operations reach the underlying index,
// distinguishing unitary writes (one scorch segment each) from batched ones.
type countingIndex struct {
	bleveIndex

	unitaryIndexes int
	unitaryDeletes int
	batches        int
}

func (c *countingIndex) Index(id string, data any) error {
	c.unitaryIndexes++
	return c.bleveIndex.Index(id, data)
}

func (c *countingIndex) Delete(id string) error {
	c.unitaryDeletes++
	return c.bleveIndex.Delete(id)
}

func (c *countingIndex) Batch(b *bleve.Batch) error {
	c.batches++
	return c.bleveIndex.Batch(b)
}

// TestIndexBatchesSections guards the ingestion cost of a document: sections
// must be written as batches, never one unitary write per section. A unitary
// write creates one scorch segment, so the segment count — and the background
// merges it triggers — would grow with the corpus and make each new document
// slower to index than the previous one.
func TestIndexBatchesSections(t *testing.T) {
	const sections = 120

	var doc strings.Builder
	doc.WriteString("---\nsource: file:///test/batching.md\n---\n\n# Batching\n\n")
	for i := range sections {
		fmt.Fprintf(&doc, "## Section %d\n\nContenu de la section %d.\n\n", i, i)
	}

	document, err := markdown.Parse([]byte(doc.String()))
	if err != nil {
		t.Fatalf("markdown.Parse() error: %+v", err)
	}

	dataDir := filepath.Join(t.TempDir(), "index.bleve")

	underlying, err := bleve.New(dataDir, IndexMapping())
	if err != nil {
		t.Fatalf("bleve.New() error: %+v", err)
	}
	t.Cleanup(func() { _ = underlying.Close() })

	counting := &countingIndex{bleveIndex: underlying}
	idx := NewIndex(counting)

	ctx := context.Background()

	if err := idx.Index(ctx, document); err != nil {
		t.Fatalf("idx.Index() error: %+v", err)
	}

	if counting.unitaryIndexes != 0 {
		t.Errorf("unitary index writes: got %d, want 0 (sections must be batched)", counting.unitaryIndexes)
	}

	// One batch per batchFlushSize sections, plus the trailing flush. The lower
	// bound also proves the counting wrapper is the index actually written to.
	maxBatches := sections/batchFlushSize + 1
	if counting.batches < 1 || counting.batches > maxBatches {
		t.Errorf("batch writes: got %d, want between 1 and %d", counting.batches, maxBatches)
	}

	// Re-indexing the same source deletes the previous sections; those deletes
	// must be batched too.
	counting.batches = 0

	if err := idx.Index(ctx, document); err != nil {
		t.Fatalf("idx.Index() (re-index) error: %+v", err)
	}

	if counting.unitaryDeletes != 0 {
		t.Errorf("unitary deletes: got %d, want 0 (deletions must be batched)", counting.unitaryDeletes)
	}

	// The deletion pass reads the previous sections back 100 hits at a time,
	// so it applies at most one batch per page on top of the indexing ones.
	maxBatches += sections/100 + 1
	if counting.batches > maxBatches {
		t.Errorf("batch writes on re-index: got %d, want at most %d", counting.batches, maxBatches)
	}
}
