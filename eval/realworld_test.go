package eval_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	amoxtli "github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/eval"
	"github.com/bornholm/amoxtli/eval/hfqa"
	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

// TestEvaluateRealWorld runs the retrieval stack against a real, multilingual
// Hugging Face QA dataset (SQuAD JSON format) and reports Recall@k / MRR /
// nDCG@k, globally and segmented by language. It is a manual benchmark, gated
// behind AMOXTLI_EVAL=1 and driven entirely by environment variables so you can
// point it at your own dataset files and embeddings endpoint.
//
// Required:
//
//	AMOXTLI_EVAL=1
//	At least one of:
//	  AMOXTLI_EVAL_SQUAD_FR=/path/to/piaf.json        (French, e.g. etalab-ia/piaf)
//	  AMOXTLI_EVAL_SQUAD_EN=/path/to/squad-dev.json   (English, e.g. rajpurkar/squad)
//	  AMOXTLI_EVAL_SQUAD_ES=/path/to/squad_es.json    (Spanish, e.g. squad_es)
//
// Optional cost bounds (per language):
//
//	AMOXTLI_EVAL_MAX_DOCS=500        cap indexed passages per language
//	AMOXTLI_EVAL_MAX_QUERIES=200     cap evaluated questions per language
//	AMOXTLI_EVAL_TOPK=10             largest cut-off
//
// Optional vector backend (adds sqlite-vec fusion on top of lexical bleve). When
// unset the benchmark runs lexical-only and needs no LLM/embeddings service:
//
//	AMOXTLI_EVAL_EMBED_BASE_URL=http://localhost:11434/v1/
//	AMOXTLI_EVAL_EMBED_MODEL=mxbai-embed-large:latest
//	AMOXTLI_EVAL_EMBED_DIM=1024       (embedding dimension; default 768)
//	AMOXTLI_EVAL_EMBED_API_KEY=...    (optional)
//	AMOXTLI_EVAL_EMBED_CACHE_DIR=...  (optional persistent on-disk embeddings
//	                                   cache, keyed by model — re-runs cost zero
//	                                   remote calls)
func TestEvaluateRealWorld(t *testing.T) {
	if os.Getenv("AMOXTLI_EVAL") == "" {
		t.Skip("set AMOXTLI_EVAL=1 to run the real-world evaluation benchmark")
	}

	// The gorm SQLite store and the sqlite-vec index both provide a WASM build to
	// ncruces/go-sqlite3; force the vec0-enabled one before any connection opens.
	sqlitevecIndex.EnsureVecWASM()

	ctx := context.Background()

	maxDocs := envInt(t, "AMOXTLI_EVAL_MAX_DOCS", 0)
	maxQueries := envInt(t, "AMOXTLI_EVAL_MAX_QUERIES", 0)
	topK := envInt(t, "AMOXTLI_EVAL_TOPK", 10)

	// Load every configured language, bound its cost, and merge into a single
	// corpus + query set.
	langs := []struct{ env, code string }{
		{"AMOXTLI_EVAL_SQUAD_FR", "fr"},
		{"AMOXTLI_EVAL_SQUAD_EN", "en"},
		{"AMOXTLI_EVAL_SQUAD_ES", "es"},
	}

	corpus := &eval.Corpus{Name: "hfqa-multilingual"}
	dataset := &eval.Dataset{Name: "hfqa-multilingual"}
	loaded := 0

	for _, l := range langs {
		path := os.Getenv(l.env)
		if path == "" {
			continue
		}
		c, ds, err := hfqa.Load(path, l.code, "squad-"+l.code)
		if err != nil {
			t.Fatalf("loading %s (%s): %+v", l.code, path, errors.WithStack(err))
		}

		c.Truncate(maxDocs)
		ds = ds.KeepAnswerable(c.Sources())
		ds.Truncate(maxQueries)

		t.Logf("loaded %s: %d passages, %d questions", l.code, len(c.Documents), len(ds.Queries))

		corpus.Merge(c)
		dataset.Queries = append(dataset.Queries, ds.Queries...)
		loaded++
	}

	if loaded == 0 {
		t.Skip("no dataset configured: set at least one of AMOXTLI_EVAL_SQUAD_{FR,EN,ES}")
	}

	t.Logf("total corpus: %d passages, %d questions across %d language(s)", len(corpus.Documents), len(dataset.Queries), loaded)

	evaluateCorpus(t, ctx, corpus, dataset, topK)
}

// evaluateCorpus builds the Codex (lexical bleve, plus sqlite-vec when an
// embeddings endpoint is configured, plus reranking when AMOXTLI_EVAL_RERANK is
// set), ingests the corpus (reusing a persisted index when AMOXTLI_EVAL_WORKDIR
// points at an already-ingested directory), evaluates the queries and logs the
// report globally and per language. Shared by the SQuAD and BEIR benchmarks.
func evaluateCorpus(t *testing.T, ctx context.Context, corpus *eval.Corpus, dataset *eval.Dataset, topK int) *eval.Report {
	t.Helper()

	// In-memory OTel pipeline collecting amoxtli's LLM instrumentation, so the
	// report states what each phase cost (calls, tokens, latency) next to the
	// quality metrics. Must be installed before the first LLM call.
	costReader := llmCostMeter()

	// Working directory: a persistent one (AMOXTLI_EVAL_WORKDIR) lets several
	// configurations (RRF weights, reranking) reuse a single ingested index
	// instead of re-embedding the corpus on every run.
	dir := os.Getenv("AMOXTLI_EVAL_WORKDIR")
	persistent := dir != ""
	if persistent {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir workdir: %+v", errors.WithStack(err))
		}
	} else {
		dir = t.TempDir()
	}
	sentinel := filepath.Join(dir, ".ingested")
	skipIngest := false
	if persistent {
		if _, err := os.Stat(sentinel); err == nil {
			skipIngest = true
		}
	}

	// RRF fusion weights per indexer (default equal weighting).
	bleveWeight := envFloat(t, "AMOXTLI_EVAL_BLEVE_WEIGHT", 0.5)
	vectorWeight := envFloat(t, "AMOXTLI_EVAL_VECTOR_WEIGHT", 0.5)

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("open bleve: %+v", errors.WithStack(err))
	}
	defer bleveIdx.Close()

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("open store: %+v", errors.WithStack(err))
	}
	defer store.Close()

	indexers := []amoxtli.Indexer{{ID: "bleve", Index: bleveIdx, Weight: bleveWeight}}
	mode := "lexical (bleve)"

	if client, embedModel := embeddingsClient(t); client != nil {
		vecDB, err := sqlite3.Open(filepath.Join(dir, "vectors.sqlite"))
		if err != nil {
			t.Fatalf("open sqlite-vec: %+v", errors.WithStack(err))
		}
		defer vecDB.Close()
		vecIdx := sqlitevecIndex.NewIndex(vecDB, client,
			sqlitevecIndex.WithEmbeddingsModel(embedModel),
			sqlitevecIndex.WithVectorSize(envInt(t, "AMOXTLI_EVAL_EMBED_DIM", 768)),
			sqlitevecIndex.WithMaxWords(envInt(t, "AMOXTLI_EVAL_EMBED_MAX_WORDS", 500)),
		)
		indexers = append(indexers, amoxtli.Indexer{ID: "vector", Index: vecIdx, Weight: vectorWeight})
		mode = fmt.Sprintf("hybrid (bleve %.2f / vector %.2f)", bleveWeight, vectorWeight)
	}

	// Feature toggles. Iterative mode drives codex.SearchIterative through the
	// orchestrator and implies HyDE + grounding (the agentic loop: query
	// expansion, evidence grounding, gated re-retrieval). Each LLM-backed
	// feature needs a chat client (AMOXTLI_EVAL_CHAT_*).
	iterative := os.Getenv("AMOXTLI_EVAL_ITERATIVE") != ""
	rerank := os.Getenv("AMOXTLI_EVAL_RERANK") != ""
	hyde := iterative || os.Getenv("AMOXTLI_EVAL_HYDE") != ""

	codexOpts := []amoxtli.Option{
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(indexers...),
		amoxtli.WithDisableJudge(),
	}
	if !hyde {
		codexOpts = append(codexOpts, amoxtli.WithDisableHyDE())
	}
	// A single chat client feeds every LLM feature (HyDE, reranker, grounding
	// evaluator, query reformulator). WithLLMClient must be set exactly once.
	if hyde || rerank || iterative {
		codexOpts = append(codexOpts, amoxtli.WithLLMClient(chatClient(t)))
	}
	if hyde {
		mode += " + hyde"
	}
	if rerank {
		codexOpts = append(codexOpts,
			amoxtli.WithReranking(),
			amoxtli.WithMaxTotalWords(envInt(t, "AMOXTLI_EVAL_RERANK_MAX_WORDS", 3000)),
		)
		mode += " + rerank"
	}
	if iterative {
		rounds := envInt(t, "AMOXTLI_EVAL_ITERATIVE_ROUNDS", 2)
		codexOpts = append(codexOpts,
			amoxtli.WithGroundingCheck(),
			amoxtli.WithGroundingMinScore(envFloat(t, "AMOXTLI_EVAL_GROUNDING_MIN_SCORE", 0.4)),
			amoxtli.WithIterativeRetrieval(rounds),
		)
		// Relevance application: "demote" ranks irrelevant evidence last instead
		// of dropping it (preserves recall@k); default "filter" removes it.
		groundingMode := "filter"
		if strings.EqualFold(os.Getenv("AMOXTLI_EVAL_GROUNDING_MODE"), "demote") {
			codexOpts = append(codexOpts, amoxtli.WithGroundingMode(retrieval.GroundingDemote))
			groundingMode = "demote"
		}
		mode += fmt.Sprintf(" + grounding(%s) + iterative(%d rounds)", groundingMode, rounds)
	}

	t.Logf("retrieval mode: %s", mode)

	codex, err := amoxtli.New(ctx, codexOpts...)
	if err != nil {
		t.Fatalf("new codex: %+v", errors.WithStack(err))
	}
	defer codex.Close()

	costBase := collectLLMCost(t, costReader)

	if skipIngest {
		t.Logf("reusing persisted index in %s (skipping ingestion)", dir)
	} else {
		collID, err := codex.CreateCollection(ctx, "hfqa")
		if err != nil {
			t.Fatalf("create collection: %+v", errors.WithStack(err))
		}

		// Ingest every passage, then wait for all indexing tasks to drain.
		ingestStart := time.Now()
		taskIDs := make([]task.ID, 0, len(corpus.Documents))
		for _, doc := range corpus.Documents {
			source, err := url.Parse(doc.Source)
			if err != nil {
				t.Fatalf("parse source %q: %+v", doc.Source, errors.WithStack(err))
			}
			filename := strings.NewReplacer("://", "_", "/", "_").Replace(doc.Source) + ".md"
			id, err := codex.IndexFile(ctx, collID, filename, strings.NewReader(passageContent(doc)),
				amoxtli.WithIndexFileSource(source),
			)
			if err != nil {
				t.Fatalf("index %q: %+v", doc.Source, errors.WithStack(err))
			}
			taskIDs = append(taskIDs, id)
		}
		waitAllTasks(t, ctx, codex, taskIDs)
		t.Logf("indexed %d passages in %s", len(corpus.Documents), time.Since(ingestStart).Round(time.Millisecond))
		costIngest := collectLLMCost(t, costReader)
		logLLMCost(t, "ingestion", costIngest.sub(costBase), 0)
		costBase = costIngest
		if persistent {
			if err := os.WriteFile(sentinel, []byte("ok\n"), 0o644); err != nil {
				t.Fatalf("write sentinel: %+v", errors.WithStack(err))
			}
		}
	}

	// Evaluate. The retriever is wrapped with a periodic progress log (same
	// throughput-based ETA as ingestion) so a slow evaluation phase — typically
	// LLM reranking, one call per query against a remote endpoint — reports how
	// far along it is instead of going silent. Evaluate runs queries
	// sequentially, so a plain counter is race-free.
	totalQueries := len(dataset.Queries)
	evalStart := time.Now()
	progressEvery := time.Duration(envInt(t, "AMOXTLI_EVAL_PROGRESS_SECONDS", 15)) * time.Second
	lastLog := evalStart
	done := 0
	retriever := eval.FromSearchResults(func(ctx context.Context, query string, k int) ([]*index.SearchResult, error) {
		var res []*index.SearchResult
		var err error
		if iterative {
			// SearchIterative runs the orchestrator: HyDE expansion, grounding
			// evaluation and gated re-retrieval, returning the fused evidence.
			var out *retrieval.Result
			out, err = codex.SearchIterative(ctx, query, amoxtli.WithSearchMaxResults(k))
			if out != nil {
				res = out.Results
			}
		} else {
			res, err = codex.Search(ctx, query, amoxtli.WithSearchMaxResults(k))
		}
		done++
		if progressEvery > 0 && time.Since(lastLog) >= progressEvery {
			elapsed := time.Since(evalStart)
			rate := float64(done) / elapsed.Seconds()
			var eta time.Duration
			if rate > 0 {
				eta = time.Duration(float64(totalQueries-done)/rate) * time.Second
			}
			t.Logf("evaluation progress: %d/%d queries (%.0f%%), elapsed %s, ~%.1f q/s, ETA %s",
				done, totalQueries, 100*float64(done)/float64(totalQueries), elapsed.Round(time.Second), rate, eta.Round(time.Second))
			lastLog = time.Now()
		}
		return res, err
	})

	report, err := eval.Evaluate(ctx, dataset, retriever, ksForTopK(topK)...)
	if err != nil {
		t.Fatalf("evaluate: %+v", errors.WithStack(err))
	}

	logLLMCost(t, "evaluation", collectLLMCost(t, costReader).sub(costBase), totalQueries)

	t.Logf("\n=== GLOBAL (%s) ===\n%s", mode, report.String())
	segments := report.ByLang()
	for _, code := range sortedKeys(segments) {
		t.Logf("\n=== %s ===\n%s", strings.ToUpper(code), segments[code].String())
	}

	// Optional end-to-end generation (reader) evaluation: plug the already-wired
	// chat client onto the retrieved passages, generate an answer per query and
	// score it (EM/F1) against the gold answers. Gated by AMOXTLI_EVAL_GENERATE
	// and only meaningful for datasets carrying gold answers (Query.Answers).
	if os.Getenv("AMOXTLI_EVAL_GENERATE") != "" {
		// A clean retrieval closure (no progress side effects) reusing the same
		// retrieval config as the metrics pass above.
		retrieveForGen := func(ctx context.Context, query string, k int) ([]string, error) {
			var res []*index.SearchResult
			var err error
			if iterative {
				var out *retrieval.Result
				out, err = codex.SearchIterative(ctx, query, amoxtli.WithSearchMaxResults(k))
				if out != nil {
					res = out.Results
				}
			} else {
				res, err = codex.Search(ctx, query, amoxtli.WithSearchMaxResults(k))
			}
			if err != nil {
				return nil, errors.WithStack(err)
			}
			ids := make([]string, 0, len(res))
			for _, r := range res {
				if r != nil && r.Source != nil {
					ids = append(ids, r.Source.String())
				}
			}
			return ids, nil
		}
		evaluateGeneration(t, ctx, corpus, dataset, retrieveForGen, mode)
	}

	// Sanity floor: a working retriever ranks a fair share of gold passages
	// near the top; MRR of exactly 0 means retrieval is completely broken.
	if report.NumQueries == 0 {
		t.Fatal("no queries evaluated")
	}
	if report.MRR <= 0 {
		t.Errorf("MRR = %.4f: retrieval returned no gold passage for any query", report.MRR)
	}

	// Non-regression floor (golden-set runs): fail when nDCG@10 drops below the
	// configured threshold.
	if floor := envFloat(t, "AMOXTLI_EVAL_MIN_NDCG10", 0); floor > 0 {
		got, ok := report.NDCGAtK[10]
		if !ok {
			t.Errorf("AMOXTLI_EVAL_MIN_NDCG10 requires AMOXTLI_EVAL_TOPK >= 10")
		} else if got < floor {
			t.Errorf("nDCG@10 = %.4f is below the non-regression floor %.4f", got, floor)
		} else {
			t.Logf("nDCG@10 = %.4f >= non-regression floor %.4f", got, floor)
		}
	}

	// One-line TSV summary appended per run, so a multi-dataset/multi-config
	// sweep (make eval-matrix) consolidates into a single table.
	if path := os.Getenv("AMOXTLI_EVAL_SUMMARY_FILE"); path != "" {
		line := fmt.Sprintf("%s\t%s\t%d\t%.4f\t%.4f\t%.4f\n",
			dataset.Name, mode, report.NumQueries, report.MRR, report.RecallAtK[topK], report.NDCGAtK[topK])
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("open summary file: %+v", errors.WithStack(err))
		}
		defer f.Close()
		if _, err := f.WriteString(line); err != nil {
			t.Fatalf("append summary: %+v", errors.WithStack(err))
		}
	}

	return report
}

// passageContent renders a document as a small markdown snippet (title as a
// heading when present) for ingestion.
func passageContent(doc eval.Document) string {
	if doc.Title != "" {
		return "# " + doc.Title + "\n\n" + doc.Content
	}
	return doc.Content
}

// embeddingsClient builds an embeddings-only LLM client from the AMOXTLI_EVAL_EMBED_*
// environment, or returns (nil, "") when no endpoint is configured (lexical-only run).
func embeddingsClient(t *testing.T) (llm.Client, string) {
	t.Helper()

	baseURL := os.Getenv("AMOXTLI_EVAL_EMBED_BASE_URL")
	model := os.Getenv("AMOXTLI_EVAL_EMBED_MODEL")
	if baseURL == "" || model == "" {
		return nil, ""
	}

	client, err := provider.Create(context.Background(),
		provider.WithEmbeddings(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: baseURL,
				Model:   model,
				APIKey:  os.Getenv("AMOXTLI_EVAL_EMBED_API_KEY"),
			},
		}),
	)
	if err != nil {
		t.Fatalf("create embeddings client: %+v", errors.WithStack(err))
	}
	// Observe inside the cache: only actual remote fetches (cache misses) are
	// counted as LLM calls in the cost report.
	return withEmbeddingsCache(t, llmx.NewObservableClient(withRetry(t, client)), model), model
}

// withEmbeddingsCache wraps client with a persistent on-disk embeddings cache
// when AMOXTLI_EVAL_EMBED_CACHE_DIR is set, keyed by the model name. The cache
// sits outside the retry/rate-limit layer so hits bypass throttling entirely —
// re-runs against an already-embedded corpus cost zero remote calls.
func withEmbeddingsCache(t *testing.T, client llm.Client, model string) llm.Client {
	t.Helper()
	dir := os.Getenv("AMOXTLI_EVAL_EMBED_CACHE_DIR")
	if dir == "" {
		return client
	}
	cache, err := llmx.NewCachingClient(client, dir, model)
	if err != nil {
		t.Fatalf("create embeddings cache: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() {
		hits, misses := cache.Stats()
		t.Logf("embeddings cache (%s): %d hits, %d misses", dir, hits, misses)
	})
	return cache
}

// withRetry wraps client with client-side rate limiting and retry/backoff so a
// remote provider's 429 (Too Many Requests) throttles and retries instead of
// dropping the passage. Both are env-tunable: AMOXTLI_EVAL_LLM_RATE (requests
// per second, 0 = unlimited) and AMOXTLI_EVAL_LLM_MAX_RETRIES.
func withRetry(t *testing.T, client llm.Client) llm.Client {
	t.Helper()
	opts := []llmx.OptionFunc{
		llmx.WithMaxRetries(envInt(t, "AMOXTLI_EVAL_LLM_MAX_RETRIES", 5)),
	}
	if r := envFloat(t, "AMOXTLI_EVAL_LLM_RATE", 0); r > 0 {
		opts = append(opts, llmx.WithRateLimit(rate.Limit(r), 1))
	}
	return llmx.NewRetryClient(client, opts...)
}

func waitAllTasks(t *testing.T, ctx context.Context, codex *amoxtli.Codex, ids []task.ID) {
	t.Helper()
	start := time.Now()
	deadline := start.Add(time.Duration(envInt(t, "AMOXTLI_EVAL_INGEST_TIMEOUT_MIN", 30)) * time.Minute)
	// A handful of passages can be un-embeddable (e.g. exceeding the embedding
	// model's context window even after chunking). Tolerate up to this many
	// failures: those passages simply stay out of the vector index (they remain
	// in bleve), rather than aborting the whole evaluation. Defaults to 0 to
	// preserve strict behaviour unless explicitly opted in.
	maxFailures := envInt(t, "AMOXTLI_EVAL_MAX_INGEST_FAILURES", 0)
	failures := 0

	// Periodic progress log with a throughput-based ETA, so a long ingestion
	// (thousands of passages, especially against a remote/throttled embeddings
	// endpoint) reports how far along it is instead of going silent.
	total := len(ids)
	progressEvery := time.Duration(envInt(t, "AMOXTLI_EVAL_PROGRESS_SECONDS", 15)) * time.Second
	lastLog := start
	logProgress := func(done int) {
		elapsed := time.Since(start)
		rate := float64(done) / elapsed.Seconds()
		var eta time.Duration
		if rate > 0 {
			eta = time.Duration(float64(total-done)/rate) * time.Second
		}
		msg := fmt.Sprintf("ingestion progress: %d/%d passages (%.0f%%), elapsed %s, ~%.1f/s, ETA %s",
			done, total, 100*float64(done)/float64(total), elapsed.Round(time.Second), rate, eta.Round(time.Second))
		if failures > 0 {
			msg += fmt.Sprintf(", %d failed", failures)
		}
		t.Log(msg)
	}

	for i, id := range ids {
		for {
			state, err := codex.TaskState(ctx, id)
			if err != nil {
				t.Fatalf("TaskState: %+v", errors.WithStack(err))
			}
			if state.Status == task.StatusSucceeded {
				break
			}
			if state.Status == task.StatusFailed {
				failures++
				if failures > maxFailures {
					t.Fatalf("indexing task %s failed (%d failures > tolerance %d): %v", id, failures, maxFailures, state.Error)
				}
				t.Logf("indexing task %s failed (%d/%d tolerated): %v", id, failures, maxFailures, state.Error)
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for indexing task %s (status %s) after %s", id, state.Status, time.Since(start).Round(time.Second))
			}
			if progressEvery > 0 && time.Since(lastLog) >= progressEvery {
				logProgress(i)
				lastLog = time.Now()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	logProgress(total)
	if failures > 0 {
		t.Logf("ingestion completed with %d tolerated failure(s)", failures)
	}
}

// chatClient builds a chat-completion LLM client for the reranker from the
// AMOXTLI_EVAL_CHAT_* environment (falling back to the embeddings base URL).
func chatClient(t *testing.T) llm.Client {
	t.Helper()
	baseURL := os.Getenv("AMOXTLI_EVAL_CHAT_BASE_URL")
	if baseURL == "" {
		baseURL = os.Getenv("AMOXTLI_EVAL_EMBED_BASE_URL")
	}
	model := os.Getenv("AMOXTLI_EVAL_CHAT_MODEL")
	if baseURL == "" || model == "" {
		t.Fatal("reranking requires AMOXTLI_EVAL_CHAT_MODEL and a chat/embed base URL")
	}
	client, err := provider.Create(context.Background(),
		provider.WithChatCompletion(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: baseURL,
				Model:   model,
				APIKey:  os.Getenv("AMOXTLI_EVAL_CHAT_API_KEY"),
			},
		}),
	)
	if err != nil {
		t.Fatalf("create chat client: %+v", errors.WithStack(err))
	}
	return withRetry(t, client)
}

func envFloat(t *testing.T, name string, def float64) float64 {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		t.Fatalf("%s must be a float, got %q", name, v)
	}
	return f
}

func envInt(t *testing.T, name string, def int) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s must be an integer, got %q", name, v)
	}
	return n
}

// ksForTopK returns the default cut-offs bounded by topK, always including topK
// itself as the largest cut-off.
func ksForTopK(topK int) []int {
	if topK < 1 {
		topK = 10
	}
	ks := []int{}
	for _, k := range eval.DefaultKs {
		if k < topK {
			ks = append(ks, k)
		}
	}
	return append(ks, topK)
}

func sortedKeys(m map[string]*eval.Report) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
