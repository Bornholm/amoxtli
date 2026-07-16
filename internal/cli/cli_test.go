package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runCLI executes the command tree in-process, capturing its output. Each
// invocation builds a fresh command so flag state never leaks between calls.
func runCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	buf := &bytes.Buffer{}

	cmd := NewRootCommand()
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)

	err := cmd.ExecuteContext(ctx)

	return buf.String(), err
}

func mustRunCLI(t *testing.T, args ...string) string {
	t.Helper()

	output, err := runCLI(t, args...)
	if err != nil {
		t.Fatalf("amoxtli %s: %v\noutput:\n%s", strings.Join(args, " "), err, output)
	}

	return output
}

const testDocument = `# Go Programming Language

Go is a statically typed, compiled programming language designed at Google.

## Concurrency

Go's concurrency model is built around goroutines and channels, inspired by
Communicating Sequential Processes (CSP). A goroutine is a lightweight thread
managed by the Go runtime.
`

const testSourceFile = `package greeting

// ParseGreetingMessage parses a greeting message and returns its recipient.
func ParseGreetingMessage(message string) string {
	return message
}
`

// TestCLILifecycle drives the full local (bleve-only, no LLM) workflow:
// init, add, search, doc management, collection stats and deletion.
func TestCLILifecycle(t *testing.T) {
	root := t.TempDir()

	docPath := filepath.Join(root, "go-intro.md")
	if err := os.WriteFile(docPath, []byte(testDocument), 0600); err != nil {
		t.Fatal(err)
	}

	// init
	output := mustRunCLI(t, "-C", root, "init")
	if !strings.Contains(output, "Initialized amoxtli workspace") {
		t.Errorf("unexpected init output: %s", output)
	}
	if _, err := os.Stat(filepath.Join(root, ".amoxtli", "config.yaml")); err != nil {
		t.Fatalf("missing config.yaml: %v", err)
	}

	// init again must fail without --force
	if _, err := runCLI(t, "-C", root, "init"); err == nil {
		t.Error("expected an error on double init")
	}
	mustRunCLI(t, "-C", root, "init", "--force")

	// add
	output = mustRunCLI(t, "-C", root, "--json", "add", "--meta", "topic=go", docPath)

	var added []struct {
		File   string `json:"file"`
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(output), &added); err != nil {
		t.Fatalf("could not parse add output %q: %v", output, err)
	}
	if len(added) != 1 || added[0].Status != "succeeded" {
		t.Fatalf("unexpected add result: %+v", added)
	}

	// add of an unsupported file must fail with exit error
	binPath := filepath.Join(root, "some.bin")
	if err := os.WriteFile(binPath, []byte{0x0}, 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := runCLI(t, "-C", root, "add", binPath); err == nil {
		t.Errorf("expected an error adding an unsupported file, got output: %s", output)
	}

	// search (human)
	output = mustRunCLI(t, "-C", root, "search", "concurrency goroutines")
	if !strings.Contains(output, "go-intro.md") {
		t.Errorf("expected a hit on go-intro.md, got: %s", output)
	}

	// search (json) with metadata filter
	output = mustRunCLI(t, "-C", root, "--json", "search", "--filter", "topic=go", "concurrency goroutines")

	var searched struct {
		Results []struct {
			Source   string  `json:"source"`
			Score    float64 `json:"score"`
			Sections []struct {
				ID      string `json:"id"`
				Content string `json:"content"`
			} `json:"sections"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(output), &searched); err != nil {
		t.Fatalf("could not parse search output %q: %v", output, err)
	}
	if len(searched.Results) == 0 || !strings.Contains(searched.Results[0].Source, "go-intro.md") {
		t.Fatalf("unexpected search results: %+v", searched)
	}
	if searched.Results[0].Score <= 0 {
		t.Errorf("expected a positive score, got %v", searched.Results[0].Score)
	}
	if len(searched.Results[0].Sections) == 0 || searched.Results[0].Sections[0].Content == "" {
		t.Errorf("expected section contents, got %+v", searched.Results[0])
	}

	// non-matching metadata filter must exclude the document
	output = mustRunCLI(t, "-C", root, "--json", "search", "--filter", "topic=rust", "concurrency goroutines")
	var filtered struct {
		Results []any `json:"results"`
	}
	if err := json.Unmarshal([]byte(output), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Results) != 0 {
		t.Errorf("expected no result with topic=rust, got %d", len(filtered.Results))
	}

	// --deep must be refused without llm.chat
	if _, err := runCLI(t, "-C", root, "search", "--deep", "anything"); err == nil || !strings.Contains(err.Error(), "llm.chat") {
		t.Errorf("expected a config error for --deep, got %v", err)
	}

	// search from a nested directory must discover the workspace upward
	nested := filepath.Join(root, "sub", "dir")
	if err := os.MkdirAll(nested, 0750); err != nil {
		t.Fatal(err)
	}
	mustRunCLI(t, "-C", nested, "search", "concurrency")

	// doc list (json) must expose the indexed document
	output = mustRunCLI(t, "-C", root, "--json", "doc", "list")

	var listed struct {
		Total     int64 `json:"total"`
		Documents []struct {
			ID     string `json:"id"`
			Source string `json:"source"`
		} `json:"documents"`
	}
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatalf("could not parse doc list output %q: %v", output, err)
	}
	if listed.Total != 1 || len(listed.Documents) != 1 {
		t.Fatalf("expected exactly one document, got %+v", listed)
	}
	docID := listed.Documents[0].ID

	// doc show
	output = mustRunCLI(t, "-C", root, "doc", "show", docID)
	if !strings.Contains(output, "go-intro.md") {
		t.Errorf("unexpected doc show output: %s", output)
	}

	// collection stats: the default collection holds the document
	output = mustRunCLI(t, "-C", root, "--json", "collection", "stats", "default")
	if !strings.Contains(output, "\"total_documents\": 1") {
		t.Errorf("expected one document in the default collection, got: %s", output)
	}

	// doc delete --dry-run must not remove anything
	mustRunCLI(t, "-C", root, "doc", "delete", "--dry-run", docID)
	output = mustRunCLI(t, "-C", root, "--json", "doc", "list")
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 {
		t.Errorf("dry-run must not delete, got total %d", listed.Total)
	}

	// doc delete for real
	mustRunCLI(t, "-C", root, "doc", "delete", "--yes", docID)
	output = mustRunCLI(t, "-C", root, "--json", "doc", "list")
	if err := json.Unmarshal([]byte(output), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 {
		t.Errorf("expected no document after delete, got total %d", listed.Total)
	}

	// add a source-code file: it must be indexed with type=code and
	// language=go metadata, filterable at search time
	codePath := filepath.Join(root, "greeting.go")
	if err := os.WriteFile(codePath, []byte(testSourceFile), 0600); err != nil {
		t.Fatal(err)
	}
	mustRunCLI(t, "-C", root, "--json", "add", codePath)

	output = mustRunCLI(t, "-C", root, "--json", "search", "--filter", "type=code", "parse greeting message")
	if err := json.Unmarshal([]byte(output), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Results) == 0 || !strings.Contains(searched.Results[0].Source, "greeting.go") {
		t.Fatalf("expected a type=code hit on greeting.go, got: %+v", searched.Results)
	}

	output = mustRunCLI(t, "-C", root, "--json", "search", "--filter", "language=go", "parse greeting message")
	if err := json.Unmarshal([]byte(output), &searched); err != nil {
		t.Fatal(err)
	}
	if len(searched.Results) == 0 || !strings.Contains(searched.Results[0].Source, "greeting.go") {
		t.Fatalf("expected a language=go hit on greeting.go, got: %+v", searched.Results)
	}

	// documentation-only filter must exclude the code file
	output = mustRunCLI(t, "-C", root, "--json", "search", "--filter", "type!=code", "parse greeting message")
	if err := json.Unmarshal([]byte(output), &searched); err != nil {
		t.Fatal(err)
	}
	for _, result := range searched.Results {
		if strings.Contains(result.Source, "greeting.go") {
			t.Errorf("type!=code returned the code file: %+v", result)
		}
	}
}

// TestCLINoWorkspace checks the error message when no workspace exists.
func TestCLINoWorkspace(t *testing.T) {
	_, err := runCLI(t, "-C", t.TempDir(), "search", "anything")
	if err == nil || !strings.Contains(err.Error(), "amoxtli init") {
		t.Errorf("expected a discovery error suggesting init, got %v", err)
	}
}
