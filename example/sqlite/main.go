// Command sqlite demonstrates amoxtli backed entirely by local files:
// a SQLite document store and a Bleve full-text index. No LLM is required —
// this is pure full-text search, runnable with a single directory argument.
//
// Usage:
//
//	go run ./example/sqlite <storage-dir>
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
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/task"
)

const demoDocument = `# Go Programming Language

Go is a statically typed, compiled programming language designed at Google.

## Concurrency

Go's concurrency model is built around goroutines and channels, inspired by
Communicating Sequential Processes (CSP). A goroutine is a lightweight thread
managed by the Go runtime.
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

	// 3. Compose the Codex. Without an LLM client, disable the HyDE/Judge
	//    transformers so the pipeline stays fully local.
	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		return err
	}
	defer codex.Close()

	return demo(ctx, codex, "concurrency goroutines")
}

// demo indexes a document and runs a query; it is shared, verbatim, with the
// postgres example.
func demo(ctx context.Context, codex *amoxtli.Codex, query string) error {
	collID, err := codex.CreateCollection(ctx, "demo")
	if err != nil {
		return err
	}
	fmt.Printf("Collection created: %s\n", collID)

	source, _ := url.Parse("example://demo/go-intro.md")
	taskID, err := codex.IndexFile(ctx, collID, "go-intro.md", strings.NewReader(demoDocument),
		amoxtli.WithIndexFileSource(source),
	)
	if err != nil {
		return err
	}

	// Indexing is asynchronous; wait for the task to finish.
	deadline := time.Now().Add(30 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			return err
		}
		if state.Status == task.StatusSucceeded {
			break
		}
		if state.Status == task.StatusFailed {
			return fmt.Errorf("indexing failed: %v", state.Error)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("indexing did not finish in time (status: %s)", state.Status)
		}
		time.Sleep(100 * time.Millisecond)
	}

	fmt.Printf("\nSearching for %q...\n", query)
	results, err := codex.Search(ctx, query, amoxtli.WithSearchMaxResults(3))
	if err != nil {
		return err
	}

	fmt.Printf("Found %d result(s):\n", len(results))
	for i, r := range results {
		fmt.Printf("  [%d] %s — %d section(s)\n", i+1, r.Source, len(r.Sections))
	}

	return nil
}
