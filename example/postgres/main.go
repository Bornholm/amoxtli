// Command postgres demonstrates amoxtli backed entirely by PostgreSQL:
// the document store and the hybrid index (index/postgres) share a single
// database. This example runs in full-text-only mode (no LLM client), so it
// needs only a reachable PostgreSQL with the `vector` and `unaccent`
// extensions available — e.g. the pgvector/pgvector Docker image.
//
// Usage:
//
//	go run ./example/postgres "postgres://user:pass@localhost:5432/kb?sslmode=disable"
package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bornholm/amoxtli"
	postgresIndex "github.com/bornholm/amoxtli/index/postgres"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/task"
	"github.com/jackc/pgx/v5/pgxpool"
)

const demoDocument = `# Go Programming Language

Go is a statically typed, compiled programming language designed at Google.

## Concurrency

Go's concurrency model is built around goroutines and channels, inspired by
Communicating Sequential Processes (CSP). A goroutine is a lightweight thread
managed by the Go runtime.
`

func main() {
	if len(os.Args) < 2 || os.Args[1] == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s <postgres-dsn>\n", os.Args[0])
		os.Exit(1)
	}

	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}

func run(dsn string) error {
	ctx := context.Background()

	// 1. PostgreSQL document store (documents, sections, collections).
	store, err := gormStore.NewPostgresStore(ctx, dsn)
	if err != nil {
		return err
	}
	defer store.Close()

	// 2. PostgreSQL index. The caller owns the pgx pool. Passing a nil LLM
	//    client keeps the index in full-text-only mode.
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	pgIdx := postgresIndex.NewIndex(pool, nil)

	// 3. Compose the Codex. No LLM client here, so disable the HyDE/Judge
	//    transformers.
	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "postgres", Index: pgIdx, Weight: 1}),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		return err
	}
	defer codex.Close()

	return demo(ctx, codex, "concurrency goroutines")
}

// demo indexes a document and runs a query; it mirrors the sqlite example.
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
