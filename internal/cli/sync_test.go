package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// syncSummary mirrors the JSON emitted by "amoxtli --json sync".
type syncSummary struct {
	BaseDir string       `json:"base_dir"`
	Indexed int          `json:"indexed"`
	Skipped int          `json:"skipped"`
	Deleted int          `json:"deleted"`
	Failed  int          `json:"failed"`
	Results []syncResult `json:"results"`
}

func mustSync(t *testing.T, root string, args ...string) syncSummary {
	t.Helper()

	output := mustRunCLI(t, append([]string{"-C", root, "--json", "sync"}, args...)...)

	var summary syncSummary
	if err := json.Unmarshal([]byte(output), &summary); err != nil {
		t.Fatalf("could not parse sync output %q: %v", output, err)
	}

	return summary
}

// listedSources returns the set of indexed document source URLs.
func listedSources(t *testing.T, root string) map[string]struct{} {
	t.Helper()

	output := mustRunCLI(t, "-C", root, "--json", "doc", "list")

	var listed struct {
		Documents []struct {
			Source string `json:"source"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatalf("could not parse doc list output %q: %v", output, err)
	}

	sources := make(map[string]struct{}, len(listed.Documents))
	for _, d := range listed.Documents {
		sources[d.Source] = struct{}{}
	}

	return sources
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestSyncBaseDir checks that --base-dir strips the prefix from the stored
// sources — the index never discloses the host's absolute paths — while the
// ETag skip and the deletion of vanished files keep working on them.
func TestSyncBaseDir(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")

	aGo := filepath.Join(src, "a.go")
	bGo := filepath.Join(src, "sub", "b.go")

	writeFile(t, aGo, "package main\n\nfunc main() {}\n")
	writeFile(t, bGo, "package sub\n\nfunc B() {}\n")

	mustRunCLI(t, "-C", root, "init")

	summary := mustSync(t, root, "--base-dir", root, src)
	if summary.Indexed != 2 || summary.Failed != 0 {
		t.Fatalf("unexpected first sync summary: %+v", summary)
	}

	sources := listedSources(t, root)
	for _, want := range []string{"file:///src/a.go", "file:///src/sub/b.go"} {
		if _, ok := sources[want]; !ok {
			t.Errorf("missing relative source %q: %v", want, sources)
		}
	}
	for source := range sources {
		if strings.Contains(source, root) {
			t.Errorf("source %q leaks the base directory %q", source, root)
		}
	}

	// The relative sources are matched back on re-sync: everything is skipped.
	summary = mustSync(t, root, "--base-dir", root, src)
	if summary.Indexed != 0 || summary.Skipped != 2 {
		t.Fatalf("expected every file skipped on re-sync, got: %+v", summary)
	}

	// A vanished file is resolved back against the base directory and purged.
	if err := os.Remove(bGo); err != nil {
		t.Fatal(err)
	}

	summary = mustSync(t, root, "--base-dir", root, src)
	if summary.Deleted != 1 || summary.Skipped != 1 {
		t.Fatalf("expected sub/b.go to be deleted, got: %+v", summary)
	}
	if _, ok := listedSources(t, root)["file:///src/sub/b.go"]; ok {
		t.Error("sub/b.go should have been deleted from the index")
	}

	// Synchronizing a tree that does not sit below the base directory has no
	// derivable source, and is refused up front.
	output, err := runCLI(t, "-C", root, "sync", "--base-dir", src, t.TempDir())
	if err == nil {
		t.Errorf("expected sync outside the base directory to fail, got: %s", output)
	}
}

// TestAddBaseDir checks the same source rewriting on explicit "add" arguments,
// including the refusal of a file outside the base directory.
func TestAddBaseDir(t *testing.T) {
	root := t.TempDir()
	docPath := filepath.Join(root, "docs", "go-intro.md")

	writeFile(t, docPath, testDocument)

	mustRunCLI(t, "-C", root, "init")
	mustRunCLI(t, "-C", root, "add", "--base-dir", root, docPath)

	if _, ok := listedSources(t, root)["file:///docs/go-intro.md"]; !ok {
		t.Errorf("expected a relative source, got: %v", listedSources(t, root))
	}

	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFile(t, outside, testDocument)

	output, err := runCLI(t, "-C", root, "add", "--base-dir", filepath.Join(root, "docs"), outside)
	if err == nil {
		t.Errorf("expected add outside the base directory to fail, got: %s", output)
	}
	if !strings.Contains(output, "outside the base directory") {
		t.Errorf("unexpected failure reason: %s", output)
	}
}

// TestSyncCommand drives the sync workflow: recursive indexing with a glob
// filter, ETag-based skipping of unchanged files, deletion of files that
// disappeared from disk, and preservation of files that are still present but
// excluded by the filter.
func TestSyncCommand(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")

	aGo := filepath.Join(src, "a.go")
	bGo := filepath.Join(src, "sub", "b.go")
	readme := filepath.Join(src, "readme.md")

	writeFile(t, aGo, "package main\n\nfunc main() {}\n")
	writeFile(t, bGo, "package sub\n\nfunc B() {}\n")
	writeFile(t, readme, "# Readme\n\nSome documentation.\n")

	mustRunCLI(t, "-C", root, "init")

	sourceURL := func(p string) string {
		return "file://" + p
	}

	// Initial sync with a glob filter: only the two .go files are indexed,
	// the .md is left out.
	summary := mustSync(t, root, "--filter", "*.go", src)
	if summary.Indexed != 2 || summary.Skipped != 0 || summary.Deleted != 0 || summary.Failed != 0 {
		t.Fatalf("unexpected first sync summary: %+v", summary)
	}

	sources := listedSources(t, root)
	if len(sources) != 2 {
		t.Fatalf("expected 2 indexed documents, got %d: %v", len(sources), sources)
	}
	if _, ok := sources[sourceURL(aGo)]; !ok {
		t.Errorf("a.go not indexed: %v", sources)
	}
	if _, ok := sources[sourceURL(bGo)]; !ok {
		t.Errorf("sub/b.go not indexed: %v", sources)
	}
	if _, ok := sources[sourceURL(readme)]; ok {
		t.Errorf("readme.md should not be indexed with --filter '*.go': %v", sources)
	}

	// Re-syncing unchanged files skips every one of them (ETag match).
	summary = mustSync(t, root, "--filter", "*.go", src)
	if summary.Indexed != 0 || summary.Skipped != 2 || summary.Deleted != 0 {
		t.Fatalf("expected all files skipped on re-sync, got: %+v", summary)
	}

	// Sync without a filter now also indexes the .md; the .go files stay
	// skipped.
	summary = mustSync(t, root, src)
	if summary.Indexed != 1 || summary.Skipped != 2 {
		t.Fatalf("expected the .md to be indexed and the .go skipped, got: %+v", summary)
	}
	if _, ok := listedSources(t, root)[sourceURL(readme)]; !ok {
		t.Fatal("readme.md should be indexed after a filterless sync")
	}

	// Modify a.go, delete sub/b.go, keep readme.md; re-sync with the .go
	// filter. a.go is re-indexed (changed), b.go is deleted (gone from disk),
	// readme.md is untouched even though the filter excludes it.
	writeFile(t, aGo, "package main\n\nfunc main() { println(\"changed\") }\n")
	if err := os.Remove(bGo); err != nil {
		t.Fatal(err)
	}

	summary = mustSync(t, root, "--filter", "*.go", src)
	if summary.Indexed != 1 || summary.Deleted != 1 || summary.Failed != 0 {
		t.Fatalf("expected 1 indexed and 1 deleted, got: %+v", summary)
	}

	sources = listedSources(t, root)
	if _, ok := sources[sourceURL(bGo)]; ok {
		t.Error("sub/b.go should have been deleted from the index")
	}
	if _, ok := sources[sourceURL(readme)]; !ok {
		t.Error("readme.md must survive: still on disk, only excluded by the filter")
	}
	if _, ok := sources[sourceURL(aGo)]; !ok {
		t.Error("a.go should remain indexed after modification")
	}

	// A dry-run reports the pending work but changes nothing.
	cGo := filepath.Join(src, "c.go")
	writeFile(t, cGo, "package main\n\nvar C = 1\n")

	output := mustRunCLI(t, "-C", root, "sync", "--filter", "*.go", "--dry-run", src)
	if !strings.Contains(output, "would index") {
		t.Errorf("dry-run should mention pending indexing, got: %s", output)
	}
	if _, ok := listedSources(t, root)[sourceURL(cGo)]; ok {
		t.Error("dry-run must not index c.go")
	}

	// Running for real now indexes the new file.
	summary = mustSync(t, root, "--filter", "*.go", src)
	if summary.Indexed != 1 || summary.Deleted != 0 {
		t.Fatalf("expected c.go to be indexed, got: %+v", summary)
	}
	if _, ok := listedSources(t, root)[sourceURL(cGo)]; !ok {
		t.Error("c.go should be indexed after the real sync")
	}
}
