package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates path (with parent dirs) holding content.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestIgnored(t *testing.T) {
	root := t.TempDir()

	// Root-level ignore file.
	writeFile(t, filepath.Join(root, FileName), "# comment\n\n*.log\nbuild/\n!keep.log\n")
	// Nested ignore file adding a rule for a deeper subtree.
	writeFile(t, filepath.Join(root, "sub", FileName), "secret.txt\n")

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"no ignore file matches plain md", filepath.Join(root, "note.md"), false},
		{"root pattern matches log", filepath.Join(root, "debug.log"), true},
		{"negation re-includes keep.log", filepath.Join(root, "keep.log"), false},
		{"directory pattern matches nested file", filepath.Join(root, "build", "app", "main.o"), true},
		{"root pattern applies in subdir", filepath.Join(root, "sub", "trace.log"), true},
		{"nested pattern matches its subtree", filepath.Join(root, "sub", "secret.txt"), true},
		{"nested pattern does not leak to root", filepath.Join(root, "secret.txt"), false},
	}

	m := New(root)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source, err := m.Ignored(tc.path)
			if err != nil {
				t.Fatalf("Ignored(%q): %+v", tc.path, err)
			}
			if got != tc.want {
				t.Fatalf("Ignored(%q) = %v (source %q), want %v", tc.path, got, source, tc.want)
			}
			if got && source == "" {
				t.Fatalf("Ignored(%q) matched but returned empty source", tc.path)
			}
		})
	}
}

func TestIgnoredNoIgnoreFiles(t *testing.T) {
	root := t.TempDir()
	m := New(root)

	got, source, err := m.Ignored(filepath.Join(root, "anything.log"))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got {
		t.Fatalf("expected not ignored without any .amoxtlignore, got source %q", source)
	}
}

func TestIgnoredOutsideRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, FileName), "*.log\n")

	// A file outside the workspace must not be governed by the root rules.
	outside := filepath.Join(t.TempDir(), "other.log")
	writeFile(t, outside, "x")

	got, _, err := New(root).Ignored(outside)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got {
		t.Fatal("file outside the workspace root should not be ignored by root rules")
	}
}
