package blob_test

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/blob"
	blobfs "github.com/bornholm/amoxtli/blob/fs"
	blobgorm "github.com/bornholm/amoxtli/blob/gorm"
	"github.com/ncruces/go-sqlite3/gormlite"
	"github.com/pkg/errors"
	"gorm.io/gorm"

	// Embed the SQLite binary so the driver is self-contained.
	_ "github.com/ncruces/go-sqlite3/embed"
)

// TestSnapshotRoundtripAcrossBackends is the point of writing the snapshotter
// against the interface: a workspace must be able to move from a filesystem
// store to a database one, and back.
func TestSnapshotRoundtripAcrossBackends(t *testing.T) {
	ctx := context.Background()

	source := newFSStore(t)
	target := newGormStore(t)

	expected := map[blob.Hash]string{}
	for _, content := range []string{"first", "second", "third"} {
		hash, err := source.Put(ctx, "image/png", []byte(content))
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}
		expected[hash] = content
	}

	snapshot, err := blob.NewSnapshotter(source).GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer snapshot.Close()

	if err := blob.NewSnapshotter(target).RestoreSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	for hash, content := range expected {
		data, info, err := target.Get(ctx, hash)
		if err != nil {
			t.Fatalf("Get(%s): %+v", hash, errors.WithStack(err))
		}

		if !bytes.Equal([]byte(content), data) {
			t.Errorf("Get(%s): expected %q, got %q", hash, content, data)
		}

		if e, g := "image/png", info.MimeType; e != g {
			t.Errorf("Get(%s) mime type: expected %q, got %q", hash, e, g)
		}
	}
}

func TestRestoreSnapshotIsAdditive(t *testing.T) {
	ctx := context.Background()

	source := newFSStore(t)
	target := newFSStore(t)

	kept, err := target.Put(ctx, "image/png", []byte("already there"))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if _, err := source.Put(ctx, "image/png", []byte("from the snapshot")); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	snapshot, err := blob.NewSnapshotter(source).GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer snapshot.Close()

	if err := blob.NewSnapshotter(target).RestoreSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if _, _, err := target.Get(ctx, kept); err != nil {
		t.Errorf("restoring must not destroy what is already stored: %+v", err)
	}

	count := 0
	if err := target.List(ctx, func(blob.Info) error {
		count++
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if e, g := 2, count; e != g {
		t.Errorf("stored blobs: expected %d, got %d", e, g)
	}
}

func TestSnapshotOfEmptyStore(t *testing.T) {
	ctx := context.Background()

	snapshot, err := blob.NewSnapshotter(newFSStore(t)).GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	defer snapshot.Close()

	data, err := io.ReadAll(snapshot)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if err := blob.NewSnapshotter(newFSStore(t)).RestoreSnapshot(ctx, bytes.NewReader(data)); err != nil {
		t.Errorf("restoring an empty snapshot: %+v", err)
	}
}

func newFSStore(t *testing.T) blob.Store {
	t.Helper()

	store, err := blobfs.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return store
}

func newGormStore(t *testing.T) blob.Store {
	t.Helper()

	db, err := gorm.Open(gormlite.Open(filepath.Join(t.TempDir(), "blobs.sqlite")), &gorm.Config{})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	return blobgorm.NewStore(db)
}
