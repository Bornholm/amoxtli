package bleve

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A recreated index is empty; the caller must know, or the corpus silently
// disappears from search results after a mapping upgrade.
func TestOpenOrCreate_ReportsRecreation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "index.bleve")

	idx, err := OpenOrCreate(ctx, path)
	if err != nil {
		t.Fatalf("OpenOrCreate: %+v", err)
	}
	if !idx.Recreated() {
		t.Error("a brand new index must report recreation")
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %+v", err)
	}

	idx, err = OpenOrCreate(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %+v", err)
	}
	if idx.Recreated() {
		t.Error("an unchanged index must not report recreation")
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %+v", err)
	}

	// A mapping change is detected through the stored hash: alter it to
	// simulate an upgrade.
	if err := os.WriteFile(filepath.Join(path, ".mapping_hash"), []byte("stale"), 0600); err != nil {
		t.Fatalf("could not overwrite mapping hash: %v", err)
	}
	idx, err = OpenOrCreate(ctx, path)
	if err != nil {
		t.Fatalf("reopen after mapping change: %+v", err)
	}
	defer idx.Close()
	if !idx.Recreated() {
		t.Error("a mapping change must report recreation")
	}
}
