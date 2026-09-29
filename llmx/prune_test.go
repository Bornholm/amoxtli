package llmx

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
)

// writeEntry writes a cache entry of size bytes last used age ago.
func writeEntry(t *testing.T, dir, name string, size int, age time.Duration) string {
	t.Helper()

	path := filepath.Join(dir, "embeddings", "ab", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("%+v", err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatalf("%+v", err)
	}

	used := time.Now().Add(-age)
	if err := os.Chtimes(path, used, used); err != nil {
		t.Fatalf("%+v", err)
	}

	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestPruneCacheRemovesEntriesUnusedForMaxAge(t *testing.T) {
	dir := t.TempDir()

	fresh := writeEntry(t, dir, "fresh.json", 10, time.Hour)
	stale := writeEntry(t, dir, "stale.json", 10, 48*time.Hour)

	stats, err := PruneCache(dir, PruneOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatalf("%+v", err)
	}

	if !exists(fresh) || exists(stale) {
		t.Errorf("fresh kept = %v, stale kept = %v; want only the fresh entry", exists(fresh), exists(stale))
	}
	if stats.Removed != 1 || stats.Freed != 10 || stats.Kept != 1 || stats.Size != 10 {
		t.Errorf("stats = %+v", stats)
	}
}

func TestPruneCacheEvictsLeastRecentlyUsedToFitMaxSize(t *testing.T) {
	dir := t.TempDir()

	oldest := writeEntry(t, dir, "oldest.json", 100, 3*time.Hour)
	older := writeEntry(t, dir, "older.json", 100, 2*time.Hour)
	recent := writeEntry(t, dir, "recent.json", 100, time.Hour)

	stats, err := PruneCache(dir, PruneOptions{MaxSize: 250})
	if err != nil {
		t.Fatalf("%+v", err)
	}

	if exists(oldest) || !exists(older) || !exists(recent) {
		t.Errorf("kept oldest = %v, older = %v, recent = %v; want only the oldest evicted", exists(oldest), exists(older), exists(recent))
	}
	if stats.Size != 200 {
		t.Errorf("size after prune = %d, want 200", stats.Size)
	}
}

func TestPruneCacheRemovesStaleTempFiles(t *testing.T) {
	dir := t.TempDir()

	stale := writeEntry(t, dir, ".tmp-stale", 10, 2*time.Hour)
	inFlight := writeEntry(t, dir, ".tmp-inflight", 10, time.Minute)

	if _, err := PruneCache(dir, PruneOptions{}); err != nil {
		t.Fatalf("%+v", err)
	}

	if exists(stale) || !exists(inFlight) {
		t.Errorf("stale kept = %v, in-flight kept = %v; want only the in-flight write kept", exists(stale), exists(inFlight))
	}
}

func TestCacheHitRecordsUse(t *testing.T) {
	e := &embedderStub{}
	c := newTestCache(t, e)
	ctx := context.Background()

	if _, err := c.Embeddings(ctx, []string{"alpha"}); err != nil {
		t.Fatalf("%+v", err)
	}

	// Age the entry as if it had not been used for a week.
	path := c.path("alpha", llm.NewEmbeddingsOptions())
	week := time.Now().Add(-7 * 24 * time.Hour)
	if err := os.Chtimes(path, week, week); err != nil {
		t.Fatalf("%+v", err)
	}

	if _, err := c.Embeddings(ctx, []string{"alpha"}); err != nil {
		t.Fatalf("%+v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Errorf("entry last use = %s, want refreshed by the hit", info.ModTime())
	}

	// A prune bounded to a day keeps it.
	if _, err := PruneCache(c.dir, PruneOptions{MaxAge: 24 * time.Hour}); err != nil {
		t.Fatalf("%+v", err)
	}
	if !exists(path) {
		t.Error("entry used by the hit was pruned")
	}
}

func TestPruneCacheEveryRunsOncePerInterval(t *testing.T) {
	dir := t.TempDir()
	opts := PruneOptions{MaxAge: 24 * time.Hour}

	if _, ran, err := PruneCacheEvery(dir, opts, time.Hour); err != nil || !ran {
		t.Fatalf("first run: ran = %v, err = %+v; want a run", ran, err)
	}

	stale := writeEntry(t, dir, "stale.json", 10, 48*time.Hour)

	if _, ran, err := PruneCacheEvery(dir, opts, time.Hour); err != nil || ran {
		t.Fatalf("second run: ran = %v, err = %+v; want it skipped", ran, err)
	}
	if !exists(stale) {
		t.Error("a skipped run removed an entry")
	}

	// Once the interval has elapsed, it runs again.
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, pruneMarker), past, past); err != nil {
		t.Fatalf("%+v", err)
	}

	if _, ran, err := PruneCacheEvery(dir, opts, time.Hour); err != nil || !ran {
		t.Fatalf("third run: ran = %v, err = %+v; want a run", ran, err)
	}
	if exists(stale) {
		t.Error("stale entry survived the run")
	}

	if _, ran, _ := PruneCacheEvery(dir, PruneOptions{}, time.Hour); ran {
		t.Error("a prune without limits ran")
	}
}
