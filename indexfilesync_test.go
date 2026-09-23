package amoxtli

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

func TestCodexIndexFileSync(t *testing.T) {
	ctx := context.Background()

	codex, collID := newBleveCodex(t)

	source, _ := url.Parse("https://example.net/sync/boeuf.md")

	var (
		mu       sync.Mutex
		progress []float32
	)

	err := codex.IndexFileSync(ctx, collID, "boeuf.md", strings.NewReader(testDocument),
		WithIndexFileSource(source),
		WithIndexFileMetadata(map[string]any{"type": "recette"}),
		WithIndexFileProgress(func(p float32, message string) {
			mu.Lock()
			progress = append(progress, p)
			mu.Unlock()
		}),
	)
	if err != nil {
		t.Fatalf("IndexFileSync: %+v", errors.WithStack(err))
	}

	// No waiting: the document must be searchable as soon as the call returns.
	results, err := codex.Search(ctx, "bourguignon", WithSearchCollections(collID))
	if err != nil {
		t.Fatalf("Search: %+v", errors.WithStack(err))
	}
	if len(results) == 0 || results[0].Source.String() != source.String() {
		t.Fatalf("expected the synchronously indexed document, got %v", results)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(progress) == 0 || progress[len(progress)-1] != 1 {
		t.Errorf("expected progress events ending at 1, got %v", progress)
	}
}

func TestCodexIndexFileSyncCanceled(t *testing.T) {
	codex, collID := newBleveCodex(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	source, _ := url.Parse("https://example.net/sync/canceled.md")

	if err := codex.IndexFileSync(ctx, collID, "canceled.md", strings.NewReader(testDocument), WithIndexFileSource(source)); err == nil {
		t.Fatal("expected an error with a canceled context")
	}

	results, err := codex.Search(context.Background(), "bourguignon", WithSearchCollections(collID))
	if err != nil {
		t.Fatalf("Search: %+v", errors.WithStack(err))
	}
	if len(results) != 0 {
		t.Errorf("a canceled indexing must not leave a document behind, got %v", results)
	}
}

func TestIndexFileStagedFileMissing(t *testing.T) {
	ctx := context.Background()

	handler := ingest.NewIndexFileHandler(nil, nil, nil, 100)
	source, _ := url.Parse("https://example.net/missing.md")

	tsk := ingest.NewIndexFileTask("/nonexistent/amoxtli-staged.md", "missing.md", "", source, []model.CollectionID{"c"}, nil)

	err := handler.Handle(ctx, tsk, make(chan task.Event, 100))
	if !errors.Is(err, ErrStagedFileMissing) {
		t.Fatalf("expected ErrStagedFileMissing, got %+v", err)
	}
}
