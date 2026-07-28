package vision

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pkg/errors"
)

func TestCachingDescriberHitAndMiss(t *testing.T) {
	inner := &countingDescriber{desc: &Description{Title: "Schéma", Description: "Un schéma."}}

	describer := newTestCache(t, inner, "model")

	first, err := describer.Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	second, err := describer.Describe(context.Background(), "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if inner.calls != 1 {
		t.Errorf("inner.calls: expected 1 (second call served from cache), got %d", inner.calls)
	}

	if e, g := first.Title, second.Title; e != g {
		t.Errorf("cached title: expected %q, got %q", e, g)
	}

	if e, g := first.Description, second.Description; e != g {
		t.Errorf("cached description: expected %q, got %q", e, g)
	}

	hits, misses := describer.Stats()
	if hits != 1 || misses != 1 {
		t.Errorf("Stats(): expected 1 hit / 1 miss, got %d / %d", hits, misses)
	}
}

func TestCachingDescriberKeyIsContentAddressed(t *testing.T) {
	inner := &countingDescriber{desc: &Description{Description: "Un schéma."}}

	describer := newTestCache(t, inner, "model")
	ctx := context.Background()

	// Same bytes, different file name and MIME type: still a hit, the key only
	// covers the content.
	if _, err := describer.Describe(ctx, "image/png", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if _, err := describer.Describe(ctx, "image/jpeg", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if inner.calls != 1 {
		t.Errorf("inner.calls: expected 1 for identical bytes, got %d", inner.calls)
	}

	// Different bytes: miss.
	if _, err := describer.Describe(ctx, "image/png", append([]byte{0}, pngPixel...)); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if inner.calls != 2 {
		t.Errorf("inner.calls: expected 2 for different bytes, got %d", inner.calls)
	}
}

func TestCachingDescriberNamespaceIsolatesEntries(t *testing.T) {
	dir := t.TempDir()
	inner := &countingDescriber{desc: &Description{Description: "Un schéma."}}
	ctx := context.Background()

	first, err := NewCachingDescriber(inner, dir, Namespace("model-a", ""))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	second, err := NewCachingDescriber(inner, dir, Namespace("model-b", ""))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if _, err := first.Describe(ctx, "image/png", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if _, err := second.Describe(ctx, "image/png", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if inner.calls != 2 {
		t.Errorf("inner.calls: expected 2 (one per namespace), got %d", inner.calls)
	}
}

func TestCachingDescriberCorruptedEntryIsAMiss(t *testing.T) {
	dir := t.TempDir()
	inner := &countingDescriber{desc: &Description{Description: "Un schéma."}}

	describer, err := NewCachingDescriber(inner, dir, "model")
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	ctx := context.Background()

	if _, err := describer.Describe(ctx, "image/png", pngPixel); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	entries, err := filepath.Glob(filepath.Join(dir, "vision", "*", "*.json"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly 1 cache entry, got %d (%+v)", len(entries), err)
	}

	if err := os.WriteFile(entries[0], []byte("{ not json"), 0o644); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	desc, err := describer.Describe(ctx, "image/png", pngPixel)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if inner.calls != 2 {
		t.Errorf("inner.calls: expected 2 (corrupted entry is a miss), got %d", inner.calls)
	}

	if e, g := "Un schéma.", desc.Description; e != g {
		t.Errorf("desc.Description: expected %q, got %q", e, g)
	}
}

func TestCachingDescriberDoesNotCacheErrors(t *testing.T) {
	inner := &countingDescriber{err: errors.New("provider unavailable")}

	describer := newTestCache(t, inner, "model")
	ctx := context.Background()

	if _, err := describer.Describe(ctx, "image/png", pngPixel); err == nil {
		t.Fatal("expected an error")
	}

	if _, err := describer.Describe(ctx, "image/png", pngPixel); err == nil {
		t.Fatal("expected an error")
	}

	if inner.calls != 2 {
		t.Errorf("inner.calls: expected 2 (a failure must not be cached), got %d", inner.calls)
	}
}

func newTestCache(t *testing.T, inner Describer, namespace string) *CachingDescriber {
	t.Helper()

	describer, err := NewCachingDescriber(inner, t.TempDir(), namespace)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	return describer
}

// countingDescriber replays a canned description and counts calls.
type countingDescriber struct {
	desc  *Description
	err   error
	calls int
}

func (d *countingDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*Description, error) {
	d.calls++

	if d.err != nil {
		return nil, d.err
	}

	return d.desc, nil
}

var _ Describer = &countingDescriber{}
