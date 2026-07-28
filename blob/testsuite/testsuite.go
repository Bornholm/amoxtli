// Package testsuite is the conformance suite every blob.Store implementation
// must pass. It exists so the filesystem and database stores are held to one
// definition of correct behaviour — the same reason index/testsuite exists for
// index.Index.
package testsuite

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/blob"
	"github.com/pkg/errors"
)

// Factory builds a fresh, empty store for one test case.
type Factory func(t *testing.T) blob.Store

// TestStore runs the whole conformance suite against the given factory.
func TestStore(t *testing.T, newStore Factory) {
	t.Run("PutGet", func(t *testing.T) { testPutGet(t, newStore) })
	t.Run("PutIsIdempotent", func(t *testing.T) { testPutIsIdempotent(t, newStore) })
	t.Run("PutRejects", func(t *testing.T) { testPutRejects(t, newStore) })
	t.Run("GetUnknown", func(t *testing.T) { testGetUnknown(t, newStore) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, newStore) })
	t.Run("List", func(t *testing.T) { testList(t, newStore) })
	t.Run("ListStopsOnError", func(t *testing.T) { testListStopsOnError(t, newStore) })
	t.Run("ConcurrentPut", func(t *testing.T) { testConcurrentPut(t, newStore) })
}

func testPutGet(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	data := []byte("the quick brown fox")

	hash, err := store.Put(ctx, "image/png", data)
	if err != nil {
		t.Fatalf("Put: %+v", errors.WithStack(err))
	}

	// The hash is the content: an implementation may not invent its own key.
	if e, g := blob.ComputeHash(data), hash; e != g {
		t.Errorf("Put hash: expected %q, got %q", e, g)
	}

	got, info, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get: %+v", errors.WithStack(err))
	}

	if !bytes.Equal(data, got) {
		t.Errorf("Get content: expected %q, got %q", data, got)
	}

	if info == nil {
		t.Fatal("Get info: expected metadata, got nil")
	}

	if e, g := hash, info.Hash; e != g {
		t.Errorf("info.Hash: expected %q, got %q", e, g)
	}

	if e, g := "image/png", info.MimeType; e != g {
		t.Errorf("info.MimeType: expected %q, got %q", e, g)
	}

	if e, g := int64(len(data)), info.Size; e != g {
		t.Errorf("info.Size: expected %d, got %d", e, g)
	}
}

func testPutIsIdempotent(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	data := []byte("same bytes")

	first, err := store.Put(ctx, "image/png", data)
	if err != nil {
		t.Fatalf("Put: %+v", errors.WithStack(err))
	}

	second, err := store.Put(ctx, "image/png", data)
	if err != nil {
		t.Fatalf("Put (again): %+v", errors.WithStack(err))
	}

	if first != second {
		t.Errorf("Put hash: expected %q on both calls, got %q", first, second)
	}

	if e, g := 1, len(list(t, store)); e != g {
		t.Errorf("stored blobs: expected %d, got %d", e, g)
	}
}

func testPutRejects(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	if _, err := store.Put(ctx, "image/png", nil); err == nil {
		t.Error("Put with empty content: expected an error")
	}

	if _, err := store.Put(ctx, "", []byte("content")); err == nil {
		t.Error("Put without a mime type: expected an error")
	}

	// The suite builds stores limited to maxBytes (see MaxBytes).
	_, err := store.Put(ctx, "image/png", bytes.Repeat([]byte("x"), int(MaxBytes)+1))
	if !errors.Is(err, blob.ErrTooLarge) {
		t.Errorf("Put oversized: expected blob.ErrTooLarge, got %+v", err)
	}

	if e, g := 0, len(list(t, store)); e != g {
		t.Errorf("stored blobs after refusals: expected %d, got %d", e, g)
	}
}

func testGetUnknown(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	unknown := blob.ComputeHash([]byte("never stored"))

	if _, _, err := store.Get(ctx, unknown); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get unknown: expected blob.ErrNotFound, got %+v", err)
	}

	// A malformed identifier must not reach the backend as-is.
	if _, _, err := store.Get(ctx, "../../etc/passwd"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get malformed: expected blob.ErrNotFound, got %+v", err)
	}
}

func testDelete(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	first := put(t, store, "image/png", []byte("first"))
	second := put(t, store, "image/jpeg", []byte("second"))

	if err := store.Delete(ctx, first); err != nil {
		t.Fatalf("Delete: %+v", errors.WithStack(err))
	}

	if _, _, err := store.Get(ctx, first); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get deleted: expected blob.ErrNotFound, got %+v", err)
	}

	if _, _, err := store.Get(ctx, second); err != nil {
		t.Errorf("Get kept: unexpected error %+v", err)
	}

	// Deleting what is already gone satisfies the caller's intent.
	if err := store.Delete(ctx, first); err != nil {
		t.Errorf("Delete unknown: unexpected error %+v", err)
	}

	if err := store.Delete(ctx); err != nil {
		t.Errorf("Delete without argument: unexpected error %+v", err)
	}
}

func testList(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	expected := map[blob.Hash]string{
		put(t, store, "image/png", []byte("first")):   "image/png",
		put(t, store, "image/jpeg", []byte("second")): "image/jpeg",
		put(t, store, "image/webp", []byte("third")):  "image/webp",
	}

	seen := map[blob.Hash]blob.Info{}

	if err := store.List(ctx, func(info blob.Info) error {
		if _, duplicate := seen[info.Hash]; duplicate {
			t.Errorf("List: blob %q enumerated twice", info.Hash)
		}

		seen[info.Hash] = info

		return nil
	}); err != nil {
		t.Fatalf("List: %+v", errors.WithStack(err))
	}

	if e, g := len(expected), len(seen); e != g {
		t.Fatalf("List: expected %d blobs, got %d", e, g)
	}

	for hash, mimeType := range expected {
		info, exists := seen[hash]
		if !exists {
			t.Errorf("List: blob %q missing", hash)
			continue
		}

		if info.MimeType != mimeType {
			t.Errorf("List: blob %q mime type: expected %q, got %q", hash, mimeType, info.MimeType)
		}

		if info.Size <= 0 {
			t.Errorf("List: blob %q size: expected a positive size, got %d", hash, info.Size)
		}
	}
}

func testListStopsOnError(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	put(t, store, "image/png", []byte("first"))
	put(t, store, "image/png", []byte("second"))
	put(t, store, "image/png", []byte("third"))

	expected := errors.New("stop")

	calls := 0
	err := store.List(ctx, func(blob.Info) error {
		calls++
		return expected
	})

	if !errors.Is(err, expected) {
		t.Errorf("List: expected the callback error, got %+v", err)
	}

	if calls != 1 {
		t.Errorf("List: expected the walk to stop after 1 call, got %d", calls)
	}
}

func testConcurrentPut(t *testing.T, newStore Factory) {
	ctx := context.Background()
	store := newStore(t)

	data := []byte("concurrently stored")

	const writers = 8

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		errs   []error
		hashes = map[blob.Hash]struct{}{}
	)

	for range writers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			hash, err := store.Put(ctx, "image/png", data)

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errs = append(errs, err)
				return
			}

			hashes[hash] = struct{}{}
		}()
	}

	wg.Wait()

	for _, err := range errs {
		t.Errorf("concurrent Put: %+v", err)
	}

	if e, g := 1, len(hashes); e != g {
		t.Errorf("concurrent Put: expected %d distinct hash, got %d", e, g)
	}

	if e, g := 1, len(list(t, store)); e != g {
		t.Errorf("stored blobs: expected %d, got %d", e, g)
	}
}

// MaxBytes is the per-blob limit the suite expects the store under test to be
// built with, so the oversized-content case is exercised without allocating a
// real 10 MiB payload.
const MaxBytes int64 = 1024

func put(t *testing.T, store blob.Store, mimeType string, data []byte) blob.Hash {
	t.Helper()

	hash, err := store.Put(context.Background(), mimeType, data)
	if err != nil {
		t.Fatalf("Put: %+v", errors.WithStack(err))
	}

	return hash
}

func list(t *testing.T, store blob.Store) []blob.Info {
	t.Helper()

	var infos []blob.Info

	if err := store.List(context.Background(), func(info blob.Info) error {
		infos = append(infos, info)
		return nil
	}); err != nil {
		t.Fatalf("List: %+v", errors.WithStack(err))
	}

	return infos
}
