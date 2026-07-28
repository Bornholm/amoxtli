// Command vision demonstrates indexing image files: each image is described by
// a vision LLM (title, detailed description, transcription of the visible
// text), and that description is indexed as ordinary markdown — so images
// become findable through the same full-text and semantic search as every
// other document, and are tagged with type=image for metadata filtering.
//
// By default the example runs offline with a fake describer, so it needs no
// API key. Point it at a real vision model with the -llm flag:
//
//	OPENROUTER_API_KEY=... go run ./example/vision -llm <storage-dir>
//
// Usage:
//
//	go run ./example/vision [-llm] [-model <model>] <storage-dir>
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/convert"
	convvision "github.com/bornholm/amoxtli/convert/vision"
	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/amoxtli/vision"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openrouter"
)

// A 1x1 transparent PNG: enough to exercise the whole pipeline. Replace it
// with a real screenshot or diagram to see what a vision model produces.
const demoImage = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="

func main() {
	useLLM := flag.Bool("llm", false, "describe the image with a real vision model (requires OPENROUTER_API_KEY)")
	model := flag.String("model", "qwen/qwen2.5-vl-72b-instruct", "vision model to use with -llm")
	flag.Parse()

	storageDir := flag.Arg(0)
	if storageDir == "" {
		fmt.Fprintf(os.Stderr, "Usage: %s [-llm] [-model <model>] <storage-dir>\n", os.Args[0])
		os.Exit(1)
	}

	if err := run(storageDir, *useLLM, *model); err != nil {
		fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		os.Exit(1)
	}
}

func run(storageDir string, useLLM bool, model string) error {
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

	// 2. Build the describer, then the image converter. The describer is
	//    wrapped in a content-addressed cache: the same image is never
	//    described twice, whatever its name or its location.
	describer, err := newDescriber(ctx, storageDir, useLLM, model)
	if err != nil {
		return err
	}

	// 3. Compose the Codex. The converter is routed by extension, exactly like
	//    the pandoc or GenAI converters: any .png/.jpg/... handed to IndexFile
	//    goes through the vision describer first.
	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		amoxtli.WithFileConverter(convert.NewRouted(convvision.NewConverter(describer))),
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

	image, err := base64.StdEncoding.DecodeString(demoImage)
	if err != nil {
		return err
	}

	if err := indexImage(ctx, codex, collID, "diagramme.png", image); err != nil {
		return err
	}

	// The image is retrieved by a word of its *description* — nothing of the
	// binary content itself is ever indexed.
	const query = "architecture diagram"

	if err := search(ctx, codex, "all documents", query); err != nil {
		return err
	}

	// type=image comes from the frontmatter emitted by the converter and
	// hoisted into the document metadata by the markdown parser.
	if err := search(ctx, codex, "images only", query,
		amoxtli.WithSearchFilter(index.Eq("type", "image"))); err != nil {
		return err
	}

	return nil
}

// newDescriber builds the image describer: a real vision model when -llm is
// set, a canned one otherwise. In both cases descriptions are cached on disk
// by image content.
func newDescriber(ctx context.Context, storageDir string, useLLM bool, model string) (vision.Describer, error) {
	var (
		describer vision.Describer
		namespace string
	)

	if useLLM {
		client, err := provider.Create(ctx, provider.WithChatCompletion(openrouter.Name, openrouter.Options{
			CommonOptions: provider.CommonOptions{
				Model:  model,
				APIKey: os.Getenv("OPENROUTER_API_KEY"),
			},
		}))
		if err != nil {
			return nil, err
		}

		// Same convention as the HyDE/Judge stages: the retry (and rate limit)
		// decoration belongs to the caller, not to the vision package.
		describer = vision.NewLLMDescriber(llmx.NewRetryClient(client))
		namespace = vision.Namespace(model, "")
	} else {
		describer = fakeDescriber{}
		namespace = "fake"
	}

	return vision.NewCachingDescriber(describer, filepath.Join(storageDir, "cache"), namespace)
}

// fakeDescriber keeps the example runnable without any API key.
type fakeDescriber struct{}

func (fakeDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*vision.Description, error) {
	return &vision.Description{
		Title:       "Architecture diagram",
		Description: "An architecture diagram showing the ingestion pipeline: files are converted to markdown, split into sections and indexed by the full-text and vector backends.",
		Text:        "ingestion → conversion → index",
	}, nil
}

var _ vision.Describer = fakeDescriber{}

// indexImage indexes one in-memory image and waits for the asynchronous task
// to finish. The extension of name routes the file to the vision converter.
func indexImage(ctx context.Context, codex *amoxtli.Codex, collID model.CollectionID, name string, content []byte) error {
	source, _ := url.Parse("example://demo/" + name)

	taskID, err := codex.IndexFile(ctx, collID, name, bytes.NewReader(content),
		amoxtli.WithIndexFileSource(source),
	)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(2 * time.Minute)
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

		sections, err := codex.GetSectionsByIDs(ctx, r.Sections)
		if err != nil {
			return err
		}

		for _, id := range r.Sections {
			section, exists := sections[id]
			if !exists {
				continue
			}

			content, err := section.Content()
			if err != nil {
				return err
			}

			fmt.Printf("      %s\n", content)
		}
	}

	return nil
}
