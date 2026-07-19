package sqlitevec

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// TestIndexAtPathConcurrentSearchesDuringIndexing exercises the read pool:
// several goroutines search concurrently while documents are being (re)indexed
// on the writer connection. Run with -race.
func TestIndexAtPathConcurrentSearchesDuringIndexing(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")

	idx, err := NewIndexAtPath(dbFile, &keywordClient{},
		WithEmbeddingsModel("mock"),
		WithVectorSize(4),
		WithReadPoolSize(3),
	)
	if err != nil {
		t.Fatalf("could not open index: %+v", errors.WithStack(err))
	}
	defer func() {
		if err := idx.Close(); err != nil {
			t.Errorf("close failed: %+v", errors.WithStack(err))
		}
	}()

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")
	indexDoc(t, ctx, idx, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n")

	const (
		searchers          = 8
		searchesPerRoutine = 10
	)

	var wg sync.WaitGroup

	// Writer leg: keep re-indexing while the searches run.
	wg.Go(func() {
		for n := range 5 {
			indexDoc(t, ctx, idx, fmt.Sprintf("mem://gamma-%d", n), "# Gamma\n\nThis section is about GAMMA topics.\n")
		}
	})

	errs := make(chan error, searchers*searchesPerRoutine)
	for range searchers {
		wg.Go(func() {
			for range searchesPerRoutine {
				results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5})
				if err != nil {
					errs <- err
					return
				}
				if len(results) == 0 {
					errs <- errors.New("no results")
					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent search failed: %+v", errors.WithStack(err))
	}
}

// TestIndexAtPathRejectsIdentityMismatchEagerly checks that reopening an
// existing index with a different embeddings model fails at construction time
// (NewIndexAtPath runs the identity check eagerly, unlike NewIndex).
func TestIndexAtPathRejectsIdentityMismatchEagerly(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")

	idx, err := NewIndexAtPath(dbFile, &keywordClient{},
		WithEmbeddingsModel("model-a"),
		WithVectorSize(4),
		WithReadPoolSize(1),
	)
	if err != nil {
		t.Fatalf("could not open index: %+v", errors.WithStack(err))
	}
	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")
	if err := idx.Close(); err != nil {
		t.Fatalf("close failed: %+v", errors.WithStack(err))
	}

	if _, err := NewIndexAtPath(dbFile, &keywordClient{},
		WithEmbeddingsModel("model-b"),
		WithVectorSize(4),
		WithReadPoolSize(1),
	); err == nil || !strings.Contains(err.Error(), "embeddings_model") {
		t.Fatalf("expected an identity mismatch error mentioning embeddings_model, got %v", err)
	}
}
