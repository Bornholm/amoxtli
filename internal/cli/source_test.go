package cli

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// TestSourceMapperAbsolute checks the pass-through mapper (no base directory):
// sources keep the absolute path, as they did before --base-dir existed.
func TestSourceMapperAbsolute(t *testing.T) {
	mapper, err := newSourceMapper("")
	if err != nil {
		t.Fatal(err)
	}

	source, err := mapper.Source("/srv/kb/docs/cli.md")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "file:///srv/kb/docs/cli.md"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}
	if got, want := mapper.Path(source), "/srv/kb/docs/cli.md"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	prefix, err := mapper.DirPrefix("/srv/kb/docs")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "file:///srv/kb/docs/"; got != want {
		t.Errorf("prefix = %q, want %q", got, want)
	}
}

// TestSourceMapperRelative checks that a configured base directory is stripped
// from the stored source and restored when resolving it back to disk.
func TestSourceMapperRelative(t *testing.T) {
	base := t.TempDir()

	mapper, err := newSourceMapper(base)
	if err != nil {
		t.Fatal(err)
	}

	file := filepath.Join(base, "docs", "cli.md")

	source, err := mapper.Source(file)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := source.String(), "file:///docs/cli.md"; got != want {
		t.Errorf("source = %q, want %q", got, want)
	}

	// The stored source must round-trip through url.Parse: a relative URL path
	// would come back with "docs" as the host.
	parsed, err := url.Parse(source.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "" {
		t.Errorf("parsed host = %q, want empty", parsed.Host)
	}
	if got := mapper.Path(parsed); got != file {
		t.Errorf("path = %q, want %q", got, file)
	}

	// The base directory itself must not yield a doubled slash.
	prefix, err := mapper.DirPrefix(base)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "file:///"; got != want {
		t.Errorf("base prefix = %q, want %q", got, want)
	}

	prefix, err = mapper.DirPrefix(filepath.Join(base, "docs"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := prefix, "file:///docs/"; got != want {
		t.Errorf("subdirectory prefix = %q, want %q", got, want)
	}
}

// TestSourceMapperOutsideBase checks that a path escaping the base directory is
// rejected instead of silently falling back to an absolute source.
func TestSourceMapperOutsideBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "kb")
	if err := os.MkdirAll(base, 0750); err != nil {
		t.Fatal(err)
	}

	mapper, err := newSourceMapper(base)
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		filepath.Join(filepath.Dir(base), "secret.md"),
		filepath.Join(base+"2", "sibling.md"), // sibling sharing the prefix
		"/etc/passwd",
	} {
		if _, err := mapper.Source(path); err == nil {
			t.Errorf("%q: expected an error, got none", path)
		}
	}
}

// TestSourceMapperInvalidBase checks the base directory validation.
func TestSourceMapperInvalidBase(t *testing.T) {
	dir := t.TempDir()

	if _, err := newSourceMapper(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected an error for a missing base directory")
	}

	file := filepath.Join(dir, "file.md")
	writeFile(t, file, "# File\n")

	if _, err := newSourceMapper(file); err == nil {
		t.Error("expected an error for a base directory that is a file")
	}
}
