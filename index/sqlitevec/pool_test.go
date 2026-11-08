package sqlitevec

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
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

// TestIndexConcurrentSearchesOnSharedConnection checks that concurrent
// searches on an index built by NewIndex, over one caller-owned connection,
// do not use that connection at the same time: a sqlite3.Conn is not safe
// for concurrent use, and the wasm module fails with an out of bounds memory
// access. Query decomposition searches its sub-questions concurrently.
func TestIndexConcurrentSearchesOnSharedConnection(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")
	indexDoc(t, ctx, idx, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n")

	const (
		searchers          = 8
		searchesPerRoutine = 20
	)

	var wg sync.WaitGroup
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

// TestSearchOnSharedConnectionHonoursTheContext checks that a search waiting
// for the shared connection gives up when its context is done, as a search
// waiting for a pooled reader does.
func TestSearchOnSharedConnectionHonoursTheContext(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})
	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")

	unlock, err := idx.lockConn(ctx)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	defer unlock()

	waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := idx.Search(waitCtx, "a question about ALPHA", index.SearchOptions{MaxResults: 5}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the search to give up with its context, got %v", err)
	}
}

// TestIndexAtPathReadsAllDuringIndexing runs All and a snapshot export while
// documents are indexed. Both read through the writer connection of
// NewIndexAtPath, which the indexing uses at the same time. Run with -race.
func TestIndexAtPathReadsAllDuringIndexing(t *testing.T) {
	ctx := context.Background()
	idx, err := NewIndexAtPath(filepath.Join(t.TempDir(), "index.sqlite"), &keywordClient{},
		WithEmbeddingsModel("mock"),
		WithVectorSize(4),
		WithReadPoolSize(2),
	)
	if err != nil {
		t.Fatalf("could not open index: %+v", errors.WithStack(err))
	}
	defer idx.Close()

	indexDoc(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n")

	var wg sync.WaitGroup
	wg.Go(func() {
		for n := range 10 {
			indexDoc(t, ctx, idx, fmt.Sprintf("mem://gamma-%d", n), "# Gamma\n\nThis section is about GAMMA topics.\n")
		}
	})
	errs := make(chan error, 20)
	for range 4 {
		wg.Go(func() {
			for range 5 {
				if err := idx.All(ctx, func(model.SectionID) bool { return true }); err != nil {
					errs <- err
					return
				}
				snapshot, err := idx.GenerateSnapshot(ctx)
				if err != nil {
					errs <- err
					return
				}
				_, err = io.Copy(io.Discard, snapshot)
				snapshot.Close()
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read failed: %+v", errors.WithStack(err))
	}
}
