package eval_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	amoxtli "github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/eval"
	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/internal/ollamatest"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcollama "github.com/testcontainers/testcontainers-go/modules/ollama"
)

const embeddingsModel = "mxbai-embed-large:latest"

// recipes are the corpus indexed by the evaluation, keyed by the source used in
// eval/testdata/recipes.json. The bodies are written so that both lexical
// (bleve) and vector (sqlitevec) retrieval have a fair chance to surface the
// right recipe for each golden query.
var recipes = map[string]string{
	"mem://recipes/air-fryer-mixed-vegetables": `# Air Fryer Mixed Vegetables

Toss chopped vegetables with oil and roast them in the air fryer at 200°C
(400°F) for 12 to 15 minutes for crispy, caramelised edges. Shake the basket
halfway through so every side gets crisp.`,
	"mem://recipes/korean-beef-bulgogi": `# Korean Beef Bulgogi

Thinly sliced beef marinated in soy sauce, sesame oil, garlic and pear. Let the
beef marinate for at least 30 minutes, or up to 4 hours in the refrigerator for
the deepest flavour, before grilling over high heat.`,
	"mem://recipes/gluten-free-artisan-bread": `# Gluten-Free Artisan Bread

A crusty gluten-free loaf made with a blend of rice flour, tapioca starch and
psyllium husk. No wheat, no gluten — just a chewy crumb and a crackling crust
baked in a Dutch oven.`,
	"mem://recipes/buttermilk-substitutes": `# Buttermilk Substitutes

Out of buttermilk? Stir one tablespoon of lemon juice or white vinegar into a
cup of milk and let it sit for five minutes. Plain yogurt thinned with milk also
works as a substitute for buttermilk in baking.`,
	"mem://recipes/crispy-roast-chicken": `# Crispy Roast Chicken

The secret to crispy chicken skin is a dry surface: pat the bird dry, salt it
and rest it uncovered in the fridge overnight, then roast at high heat so the
skin renders and crisps.`,
}

func requireOllama(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: requires docker + ollama")
	}
	if os.Getenv("AMOXTLI_TEST_OLLAMA") == "" {
		t.Skip("set AMOXTLI_TEST_OLLAMA=1 to run (requires docker + ollama)")
	}
}

// TestEvaluateCodex ingests the recipe corpus into a real Codex (bleve +
// sqlitevec fusion, Ollama embeddings) and scores it against the golden dataset,
// asserting the retrieval quality clears a sane floor. It is the manual
// counterpart of the unit metrics tests and the place to compare configurations
// (e.g. lexical-only vs. hybrid fusion) by reading the logged report.
func TestEvaluateCodex(t *testing.T) {
	requireOllama(t)

	// The gorm SQLite store and the sqlite-vec index both provide a WASM build to
	// ncruces/go-sqlite3; force the vec0-enabled one before any connection opens.
	sqlitevecIndex.EnsureVecWASM()

	ctx := context.Background()
	client := newOllamaClient(t)
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	vecDB, err := sqlite3.Open(filepath.Join(dir, "vectors.sqlite"))
	if err != nil {
		t.Fatalf("could not open sqlite-vec db: %+v", errors.WithStack(err))
	}
	defer vecDB.Close()
	vecIdx := sqlitevecIndex.NewIndex(vecDB, client,
		sqlitevecIndex.WithEmbeddingsModel(embeddingsModel),
		sqlitevecIndex.WithMaxWords(500),
	)

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(
			amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.4},
			amoxtli.Indexer{ID: "vector", Index: vecIdx, Weight: 0.6},
		),
		amoxtli.WithLLMClient(client),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	collID, err := codex.CreateCollection(ctx, "recipes")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	for src, body := range recipes {
		source, err := url.Parse(src)
		if err != nil {
			t.Fatalf("parse source %q: %+v", src, errors.WithStack(err))
		}
		taskID, err := codex.IndexFile(ctx, collID, filepath.Base(source.Path)+".md", strings.NewReader(body),
			amoxtli.WithIndexFileSource(source),
		)
		if err != nil {
			t.Fatalf("could not index %q: %+v", src, errors.WithStack(err))
		}
		waitTask(t, ctx, codex, taskID)
	}

	ds, err := eval.LoadDataset("testdata/recipes.json")
	if err != nil {
		t.Fatalf("LoadDataset: %+v", errors.WithStack(err))
	}

	retriever := eval.FromSearchResults(func(ctx context.Context, query string, k int) ([]*index.SearchResult, error) {
		return codex.Search(ctx, query, amoxtli.WithSearchMaxResults(k))
	})

	report, err := eval.Evaluate(ctx, ds, retriever, 1, 3, 5)
	if err != nil {
		t.Fatalf("Evaluate: %+v", errors.WithStack(err))
	}

	t.Logf("\n%s", report.String())

	// Sanity floor: with a matched corpus the target should land in the top-3
	// for most queries and the ranking (MRR) should be well above random.
	if report.RecallAtK[3] < 0.6 {
		t.Errorf("Recall@3 = %.3f, want >= 0.6", report.RecallAtK[3])
	}
	if report.MRR < 0.4 {
		t.Errorf("MRR = %.3f, want >= 0.4", report.MRR)
	}
}

func waitTask(t *testing.T, ctx context.Context, codex *amoxtli.Codex, taskID task.ID) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		state, err := codex.TaskState(ctx, taskID)
		if err != nil {
			t.Fatalf("TaskState: %+v", errors.WithStack(err))
		}
		if state.Status == task.StatusSucceeded {
			return
		}
		if state.Status == task.StatusFailed {
			t.Fatalf("indexing task failed: %v", state.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for task %s (status %s)", taskID, state.Status)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func newOllamaClient(t *testing.T) llm.Client {
	t.Helper()
	ctx := context.Background()

	ollamaContainer, err := tcollama.Run(ctx, "ollama/ollama:0.5.7", testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Mounts: testcontainers.ContainerMounts{
				{
					Source: testcontainers.GenericVolumeMountSource{Name: "ollama-data"},
					Target: "/root/.ollama",
				},
			},
		},
	}))
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ollamaContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("failed to start container: %+v", err)
	}

	ollamatest.EnsureModels(t, ctx, ollamaContainer, embeddingsModel)

	connectionStr, err := ollamaContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	client, err := provider.Create(ctx,
		provider.WithEmbeddings(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   embeddingsModel,
			},
		}),
	)
	if err != nil {
		t.Fatalf("failed to create llm client: %+v", errors.WithStack(err))
	}

	return client
}
