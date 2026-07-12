// Command convert demonstrates two things at once:
//
//   - File conversion: a non-markdown document (here HTML) is converted to
//     markdown before being parsed and indexed. Conversion is wired through
//     amoxtli.WithFileConverter; the ingestion pipeline invokes it
//     automatically for any extension the converter supports.
//   - Task tracking: IndexFile is asynchronous and returns a task.ID. This
//     example polls the task state and prints the live progress and the
//     step messages emitted by the pipeline ("converting document",
//     "parsing document", "indexing document", ...).
//
// To keep it runnable with no external binary, the converter below is a tiny
// self-contained HTML→markdown transform. In a real deployment you would
// typically use one of the bundled converters instead, e.g.:
//
//	import "github.com/bornholm/amoxtli/convert"
//	import "github.com/bornholm/amoxtli/convert/pandoc"
//	amoxtli.WithFileConverter(convert.NewRouted(pandoc.NewConverter()))
//
// (pandoc handles .docx, .odt, .rtf, .epub, .html, .tex, ... — see
// convert/pandoc.)
//
// Usage:
//
//	go run ./example/convert <storage-dir>
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/convert"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/task"
)

// The document we ingest is HTML, not markdown: it must be converted first.
const demoHTML = `<html>
<body>
<h1>Go Programming Language</h1>
<p>Go is a <strong>statically typed</strong>, compiled programming language
designed at Google.</p>
<h2>Concurrency</h2>
<p>Go's concurrency model is built around <em>goroutines</em> and channels,
inspired by Communicating Sequential Processes.</p>
</body>
</html>`

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

	// 1. SQLite document store + Bleve full-text index (see example/sqlite).
	store, err := gormStore.NewSQLiteStore(filepath.Join(storageDir, "data.sqlite"))
	if err != nil {
		return err
	}
	defer store.Close()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(storageDir, "index.bleve"))
	if err != nil {
		return err
	}
	defer bleveIdx.Close()

	// 2. Compose the Codex with a file converter. Any convert.Converter works;
	//    here a self-contained one, but convert.NewRouted(pandoc.NewConverter())
	//    would plug in pandoc the same way.
	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		amoxtli.WithFileConverter(&htmlConverter{}),
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

	// 3. Show the conversion concretely: the same converter that the pipeline
	//    runs internally can also be called directly. This is only to make the
	//    "HTML → markdown" step visible; IndexFile below does it for you.
	converted, err := (&htmlConverter{}).Convert(ctx, "report.html", strings.NewReader(demoHTML))
	if err != nil {
		return err
	}
	md, _ := io.ReadAll(converted)
	converted.Close()
	fmt.Printf("\n--- converted markdown ---\n%s--------------------------\n\n", md)

	// 4. Index the HTML file. Because "report.html" is not markdown, the
	//    pipeline routes it through the converter before parsing/indexing.
	source, _ := url.Parse("example://demo/report.html")
	taskID, err := codex.IndexFile(ctx, collID, "report.html", strings.NewReader(demoHTML),
		amoxtli.WithIndexFileSource(source),
	)
	if err != nil {
		return err
	}
	fmt.Printf("Indexing task scheduled: %s\n\n", taskID)

	// 5. Track the task: poll its state and print progress + step messages as
	//    they change. This is the same mechanism a UI would use to show a
	//    progress bar. Note: for a tiny document the whole pipeline finishes
	//    between two polls, so intermediate messages ("converting document",
	//    "parsing document", ...) may be coalesced into the final state.
	if err := waitForTask(ctx, codex, taskID); err != nil {
		return err
	}

	// 6. The converted document is now searchable.
	const query = "concurrency goroutines"
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

// waitForTask polls a task until it succeeds or fails, printing each distinct
// progress/message update along the way.
func waitForTask(ctx context.Context, codex *amoxtli.Codex, id task.ID) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastLine string

	for {
		state, err := codex.TaskState(ctx, id)
		if err != nil {
			return err
		}

		// Only print when the displayed line actually changes.
		line := fmt.Sprintf("[%3.0f%%] %s", state.Progress*100, state.Message)
		if line != lastLine {
			fmt.Println(line)
			lastLine = line
		}

		switch state.Status {
		case task.StatusSucceeded:
			return nil
		case task.StatusFailed:
			return fmt.Errorf("indexing failed: %v", state.Error)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("indexing did not finish in time (status: %s)", state.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// htmlConverter is a minimal, dependency-free convert.Converter turning a
// handful of HTML tags into markdown. It exists only to keep this example
// self-contained; prefer convert/pandoc (or convert/genai) in real use.
type htmlConverter struct{}

// SupportedExtensions implements convert.Converter.
func (c *htmlConverter) SupportedExtensions() []string {
	return []string{".html", ".htm"}
}

var (
	reH1     = regexp.MustCompile(`(?is)<h1[^>]*>(.*?)</h1>`)
	reH2     = regexp.MustCompile(`(?is)<h2[^>]*>(.*?)</h2>`)
	reStrong = regexp.MustCompile(`(?is)<(?:strong|b)(?:\s[^>]*)?>(.*?)</(?:strong|b)>`)
	reEm     = regexp.MustCompile(`(?is)<(?:em|i)(?:\s[^>]*)?>(.*?)</(?:em|i)>`)
	reP      = regexp.MustCompile(`(?is)<p[^>]*>(.*?)</p>`)
	reTags   = regexp.MustCompile(`(?is)<[^>]+>`)
	reBlanks = regexp.MustCompile(`\n{3,}`)
)

// Convert implements convert.Converter.
func (c *htmlConverter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}

	md := string(data)
	md = reH1.ReplaceAllString(md, "\n# $1\n")
	md = reH2.ReplaceAllString(md, "\n## $1\n")
	md = reStrong.ReplaceAllString(md, "**$1**")
	md = reEm.ReplaceAllString(md, "*$1*")
	md = reP.ReplaceAllString(md, "\n$1\n")
	md = reTags.ReplaceAllString(md, "") // drop any remaining tags
	md = reBlanks.ReplaceAllString(strings.TrimSpace(md), "\n\n")

	return io.NopCloser(strings.NewReader(md + "\n")), nil
}

var _ convert.Converter = &htmlConverter{}
