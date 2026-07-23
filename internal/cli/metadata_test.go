package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/internal/filternorm"
)

// listedMetadata returns the metadata of every indexed document, keyed by
// source URL.
func listedMetadata(t *testing.T, root string) map[string]map[string]any {
	t.Helper()

	output := mustRunCLI(t, "-C", root, "--json", "doc", "list")

	var listed struct {
		Documents []struct {
			Source   string         `json:"source"`
			Metadata map[string]any `json:"metadata"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatalf("could not parse doc list output %q: %v", output, err)
	}

	metadata := make(map[string]map[string]any, len(listed.Documents))
	for _, d := range listed.Documents {
		metadata[d.Source] = d.Metadata
	}

	return metadata
}

// TestAddFileMetadata checks the attributes "add" derives from the file itself,
// that --meta overrides them, and that --no-file-metadata suppresses them.
func TestAddFileMetadata(t *testing.T) {
	root := t.TempDir()
	docPath := filepath.Join(root, "docs", "go-intro.md")

	writeFile(t, docPath, testDocument)

	info, err := os.Stat(docPath)
	if err != nil {
		t.Fatal(err)
	}

	mustRunCLI(t, "-C", root, "init")
	mustRunCLI(t, "-C", root, "add", "--base-dir", root, docPath)

	metadata := listedMetadata(t, root)["file:///docs/go-intro.md"]
	if metadata == nil {
		t.Fatalf("document not found: %v", listedMetadata(t, root))
	}

	for key, want := range map[string]any{
		"filename":  "go-intro.md",
		"extension": "md",
		// JSON numbers decode as float64.
		"size":    float64(len(testDocument)),
		"mtime":   filternorm.FormatTime(info.ModTime()),
		"dirname": "/docs",
	} {
		if got := metadata[key]; got != want {
			t.Errorf("metadata %q: expected %v, got %v", key, want, got)
		}
	}

	// indexed_at describes the run, not the file, so it is only checked for
	// being a canonical date close to now.
	indexedAt, ok := metadata["indexed_at"].(string)
	if !ok {
		t.Fatalf("expected an indexed_at string, got %v", metadata["indexed_at"])
	}
	parsed, err := time.Parse(filternorm.CanonicalTimeLayout, indexedAt)
	if err != nil {
		t.Fatalf("indexed_at %q is not canonical: %v", indexedAt, err)
	}
	if time.Since(parsed) > time.Hour {
		t.Errorf("indexed_at %q is not the current run", indexedAt)
	}

	// The derived values are filterable, which is the point of indexing them.
	output := mustRunCLI(t, "-C", root, "--json", "search", "--filter", "extension=md", "--filter", "size>0", "concurrency goroutines")
	if !strings.Contains(output, "go-intro.md") {
		t.Errorf("expected a hit filtered on the derived metadata, got: %s", output)
	}

	// A --meta pair wins over the derived value of the same key.
	other := filepath.Join(root, "docs", "override.md")
	writeFile(t, other, testDocument)
	mustRunCLI(t, "-C", root, "add", "--base-dir", root, "--meta", "extension=custom", other)

	if got := listedMetadata(t, root)["file:///docs/override.md"]["extension"]; got != "custom" {
		t.Errorf("expected --meta to override the derived extension, got %v", got)
	}

	// --no-file-metadata leaves only what the caller passed explicitly.
	bare := filepath.Join(root, "docs", "bare.md")
	writeFile(t, bare, testDocument)
	mustRunCLI(t, "-C", root, "add", "--base-dir", root, "--no-file-metadata", "--meta", "topic=go", bare)

	bareMetadata := listedMetadata(t, root)["file:///docs/bare.md"]
	if got := bareMetadata["topic"]; got != "go" {
		t.Errorf("expected the explicit metadata to survive, got %v", got)
	}
	for _, key := range []string{"filename", "extension", "size", "mtime", "dirname", "indexed_at"} {
		if _, exists := bareMetadata[key]; exists {
			t.Errorf("--no-file-metadata should not have attached %q: %v", key, bareMetadata)
		}
	}
}

// TestSyncFileMetadata checks that a synced tree carries the same derived
// attributes, with dirname reported relative to --base-dir.
func TestSyncFileMetadata(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")

	writeFile(t, filepath.Join(src, "a.go"), "package main\n\nfunc main() {}\n")
	writeFile(t, filepath.Join(src, "sub", "b.go"), "package sub\n\nfunc B() {}\n")

	mustRunCLI(t, "-C", root, "init")

	if summary := mustSync(t, root, "--base-dir", root, src); summary.Indexed != 2 {
		t.Fatalf("unexpected sync summary: %+v", summary)
	}

	metadata := listedMetadata(t, root)

	for source, wantDir := range map[string]string{
		"file:///src/a.go":     "/src",
		"file:///src/sub/b.go": "/src/sub",
	} {
		if got := metadata[source]["dirname"]; got != wantDir {
			t.Errorf("%s: expected dirname %q, got %v", source, wantDir, got)
		}
		if got := metadata[source]["extension"]; got != "go" {
			t.Errorf("%s: expected extension \"go\", got %v", source, got)
		}
		// The source code parser still tags the document; the derived
		// attributes are added alongside, not instead.
		if got := metadata[source]["language"]; got != "go" {
			t.Errorf("%s: expected the parser metadata to survive, got %v", source, got)
		}
	}

	if got := metadata["file:///src/a.go"]["filename"]; got != "a.go" {
		t.Errorf("expected filename \"a.go\", got %v", got)
	}

	// --no-file-metadata suppresses the derived attributes on sync too.
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, "docs", "c.md"), testDocument)
	mustRunCLI(t, "-C", bare, "init")
	mustSync(t, bare, "--base-dir", bare, "--no-file-metadata", filepath.Join(bare, "docs"))

	if _, exists := listedMetadata(t, bare)["file:///docs/c.md"]["filename"]; exists {
		t.Errorf("--no-file-metadata should not have attached filename: %v", listedMetadata(t, bare))
	}
}
