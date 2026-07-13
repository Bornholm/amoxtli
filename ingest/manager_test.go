package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagerStagingDirLifecycle(t *testing.T) {
	m := &Manager{}

	dir, err := m.stagingDir()
	if err != nil {
		t.Fatalf("stagingDir: %+v", err)
	}
	if dir == "" {
		t.Fatal("stagingDir returned an empty path")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("staging dir does not exist: %+v", err)
	}

	// Second call returns the same directory (created once).
	dir2, err := m.stagingDir()
	if err != nil {
		t.Fatalf("stagingDir (2nd): %+v", err)
	}
	if dir2 != dir {
		t.Errorf("stagingDir returned a different path on second call: %s != %s", dir2, dir)
	}

	// Drop a file to make sure CleanupTempDir removes the whole tree.
	if err := os.WriteFile(filepath.Join(dir, "staged.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatalf("could not write staged file: %+v", err)
	}

	if err := m.CleanupTempDir(); err != nil {
		t.Fatalf("CleanupTempDir: %+v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("staging dir still present after CleanupTempDir: err=%v", err)
	}
}

func TestCleanupTempDirNoop(t *testing.T) {
	// A manager that never staged a file must not error on cleanup.
	m := &Manager{}
	if err := m.CleanupTempDir(); err != nil {
		t.Fatalf("CleanupTempDir on unused manager: %+v", err)
	}
}

func TestCleanStaleTempDirs(t *testing.T) {
	base := os.TempDir()

	// A stale directory (old mtime) must be removed.
	stale, err := os.MkdirTemp(base, tempDirPrefix+"*")
	if err != nil {
		t.Fatalf("could not create stale dir: %+v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("could not backdate stale dir: %+v", err)
	}

	// A fresh directory (recent mtime) must be preserved.
	fresh, err := os.MkdirTemp(base, tempDirPrefix+"*")
	if err != nil {
		t.Fatalf("could not create fresh dir: %+v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(fresh) })

	cleanStaleTempDirs(24 * time.Hour)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staging dir was not removed: err=%v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh staging dir was wrongly removed: err=%v", err)
	}
}
