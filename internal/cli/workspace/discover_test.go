package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscover(t *testing.T) {
	root := t.TempDir()

	amoxtliDir := filepath.Join(root, DirName)
	nested := filepath.Join(root, "sub", "deeper")

	if err := os.MkdirAll(amoxtliDir, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0750); err != nil {
		t.Fatal(err)
	}

	for _, start := range []string{root, nested} {
		ws, err := Discover(start)
		if err != nil {
			t.Fatalf("Discover(%q): %+v", start, err)
		}
		if ws.Root != root {
			t.Errorf("Discover(%q): expected root %q, got %q", start, root, ws.Root)
		}
		if ws.Dir != amoxtliDir {
			t.Errorf("Discover(%q): expected dir %q, got %q", start, amoxtliDir, ws.Dir)
		}
	}
}

func TestDiscoverNearestWins(t *testing.T) {
	root := t.TempDir()

	inner := filepath.Join(root, "sub")
	for _, dir := range []string{filepath.Join(root, DirName), filepath.Join(inner, DirName)} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			t.Fatal(err)
		}
	}

	ws, err := Discover(filepath.Join(inner))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if ws.Root != inner {
		t.Errorf("expected nearest workspace %q, got %q", inner, ws.Root)
	}
}

func TestDiscoverIgnoresFile(t *testing.T) {
	root := t.TempDir()

	outer := filepath.Join(root, DirName)
	if err := os.MkdirAll(outer, 0750); err != nil {
		t.Fatal(err)
	}

	// A plain file named .amoxtli must not shadow the real directory above.
	inner := filepath.Join(root, "sub")
	if err := os.MkdirAll(inner, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inner, DirName), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}

	ws, err := Discover(inner)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if ws.Dir != outer {
		t.Errorf("expected %q, got %q", outer, ws.Dir)
	}
}

func TestDiscoverNotFound(t *testing.T) {
	_, err := Discover(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDiscoverEnvOverride(t *testing.T) {
	root := t.TempDir()

	amoxtliDir := filepath.Join(root, DirName)
	if err := os.MkdirAll(amoxtliDir, 0750); err != nil {
		t.Fatal(err)
	}

	t.Setenv(EnvDir, amoxtliDir)

	// Discovery must not walk the tree: start from an unrelated directory.
	ws, err := Discover(t.TempDir())
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if ws.Dir != amoxtliDir {
		t.Errorf("expected %q, got %q", amoxtliDir, ws.Dir)
	}

	t.Setenv(EnvDir, filepath.Join(root, "does-not-exist"))

	if _, err := Discover(root); err == nil {
		t.Error("expected an error for a dangling AMOXTLI_DIR")
	}
}

func TestResolve(t *testing.T) {
	ws := New(filepath.Join("/project", DirName))

	if got := ws.Resolve("data/store.sqlite"); got != filepath.Join("/project", DirName, "data", "store.sqlite") {
		t.Errorf("unexpected relative resolution: %q", got)
	}
	if got := ws.Resolve("/absolute/path.sqlite"); got != "/absolute/path.sqlite" {
		t.Errorf("absolute paths must pass through, got %q", got)
	}
}
