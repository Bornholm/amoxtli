// Command sourcecode demonstrates indexing source code alongside
// documentation and searching across both. A Markdown document and a Go source
// file are indexed into the same collection; the source file is parsed with
// tree-sitter (pure Go) into declaration-level sections and automatically
// tagged with type=code and language=go metadata. The same query is then run
// three ways — unfiltered, code only, documentation only — showing how the
// metadata filter separates the two sources (e.g. to cross-reference a doc
// claim against its implementation).
//
// No LLM and no external binary are required.
//
// Usage:
//
//	go run ./example/sourcecode <storage-dir>
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
)

// The documentation claims the greeting is uppercased...
const demoDoc = `# Greeting service

The service exposes a single entry point, ` + "`FormatGreeting`" + `, which builds
a greeting for the given name. The returned message is always uppercased.
`

// ...but the implementation does not uppercase anything — exactly the kind of
// doc/code contradiction cross-search is meant to surface.
const demoCode = `package greeting

import "fmt"

// FormatGreeting builds a greeting message for the given name.
func FormatGreeting(name string) string {
	return fmt.Sprintf("Hello, %s!", name)
}
`

func main() {
	storageDir := os.Args[len(os.Args)-1]
	if len(os.Args) < 2 || storageDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s <storage-dir>\n", os.Args[0])
		os.Exit(1)
	}

	if err := run(storageDir); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}

func run(storageDir string) error {
	ctx := context.Background()

	if err := os.MkdirAll(storageDir, 0750); err != nil {
		return err
	}

	// 1. SQLite document store (documents, sections, collections).
	store, err := gormStore.NewSQLiteStore(filepath.Join(storageDir, "data.sqlite"))
	if err != nil {
		return err
	}
	defer store.Close()

	// 2. Bleve full-text index.
	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(storageDir, "index.bleve"))
	if err != nil {
		return err
	}
	defer bleveIdx.Close()

	// 3. Compose the Codex. WithSourceCode enables tree-sitter parsing for the
	//    registered extensions (.go, .js/.ts, .py, .php...). Without an LLM
	//    client, disable the HyDE/Judge transformers so the pipeline stays
	//    fully local.
	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		amoxtli.WithSourceCode(sourcecode.DefaultRegistry()),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		return err
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "demo")
	if err != nil {
		return err
	}
	fmt.Printf("Collection created: %s\n", collID)

	// Index the documentation (Markdown) and the implementation (Go). The .go
	// extension routes the file through the source-code parser automatically.
	if err := indexFile(ctx, codex, collID, "greeting.md", demoDoc); err != nil {
		return err
	}
	if err := indexFile(ctx, codex, collID, "greeting.go", demoCode); err != nil {
		return err
	}

	const query = "greeting message format"

	// Unfiltered: both sources match.
	if err := search(ctx, codex, "both doc and code", query); err != nil {
		return err
	}

	// Code only (type=code is injected automatically by the source-code parser).
	if err := search(ctx, codex, "code only", query,
		amoxtli.WithSearchFilter(index.Eq("type", "code"))); err != nil {
		return err
	}

	// Documentation only: Markdown documents carry no type at all. index.Ne
	// requires the key to be present (SQL NULL-like semantics), so absence is
	// expressed with index.NotExists.
	if err := search(ctx, codex, "documentation only", query,
		amoxtli.WithSearchFilter(index.NotExists("type"))); err != nil {
		return err
	}

	return nil
}

// indexFile indexes one in-memory file and waits for the asynchronous task to
// finish. The extension of name drives the parser selection (source code vs
// Markdown).
func indexFile(ctx context.Context, codex *amoxtli.Codex, collID model.CollectionID, name, content string) error {
	source, _ := url.Parse("example://demo/" + name)
	taskID, err := codex.IndexFile(ctx, collID, name, strings.NewReader(content),
		amoxtli.WithIndexFileSource(source),
	)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			return err
		}
		if state.Status == task.StatusSucceeded {
			fmt.Printf("Indexed %s\n", name)
			return nil
		}
		if state.Status == task.StatusFailed {
			return fmt.Errorf("indexing %s failed: %v", name, state.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("indexing %s did not finish in time (status: %s)", name, state.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func search(ctx context.Context, codex *amoxtli.Codex, label, query string, opts ...amoxtli.SearchOption) error {
	opts = append(opts, amoxtli.WithSearchMaxResults(5))

	results, err := codex.Search(ctx, query, opts...)
	if err != nil {
		return err
	}

	fmt.Printf("\n[%s] %q — %d result(s):\n", label, query, len(results))
	for i, r := range results {
		fmt.Printf("  [%d] %s — %d section(s)\n", i+1, r.Source, len(r.Sections))
	}

	return nil
}
