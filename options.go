package amoxtli

import (
	"net/url"
	"time"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/genai/llm"
)

// Indexer identifies a weighted index.Index inside the search pipeline.
type Indexer struct {
	// ID identifies the indexer in the pipeline (e.g. "bleve", "postgres").
	ID string
	// Index is any implementation of the index.Index contract.
	Index index.Index
	// Weight is the relative weight of this indexer when merging scores.
	Weight float64
}

type options struct {
	llmClient          llm.Client
	fileConverter      convert.Converter
	sourceCode         *sourcecode.Registry
	maxWordsPerSection int
	maxTotalWords      int
	taskParallelism    int
	disableHyDE        bool
	disableJudge       bool
	snapshotBoundary   string
	// Grounding & explicit re-retrieval (see CheckGrounding / SearchIterative).
	groundingCheck             bool
	groundingFailOpen          bool
	groundingMinScore          float64
	groundingMode              retrieval.GroundingMode
	iterativeRetrieval         bool
	iterativeMaxRounds         int
	queryDecomposition         bool
	decompositionMaxSubQueries int
	// Reranking reorders fused results with an LLM before pagination.
	reranking bool
	// Indexers composing the search pipeline.
	indexers []Indexer
	// Explicit components.
	index      index.Index
	store      ingest.Store
	taskRunner task.Runner
	// closeTimeout bounds how long Close waits for in-flight tasks to drain.
	closeTimeout time.Duration
	// persistentTasks enables a gorm-backed task runner sharing the store's
	// database; stagingDir is the stable staging directory tasks are resumed
	// from.
	persistentTasks bool
	stagingDir      string
	// observability wraps the LLM client with the OpenTelemetry decorator so its
	// calls emit spans and token/latency metrics.
	observability bool
}

func defaultOptions() *options {
	return &options{
		maxWordsPerSection: 250,
		// 8000 words map to roughly 14k tokens at the ~1.8 tokens/word measured
		// on mixed French prose and source code, leaving comfortable headroom
		// below the common 32k-token context limit of chat endpoints once the
		// system prompt, JSON schema and completion are accounted for (the
		// previous 12000-word default flirted with 22k tokens). It also bounds
		// the cost of every LLM retrieval stage (judge, reranker, evidence
		// evaluator), which is billed per search.
		maxTotalWords:              8000,
		taskParallelism:            5,
		snapshotBoundary:           "amoxtli-snapshot-v1",
		groundingMinScore:          0.4,
		iterativeMaxRounds:         1,
		decompositionMaxSubQueries: 3,
		closeTimeout:               30 * time.Second,
	}
}

// Option is a function that configures a Codex instance.
type Option func(*options)

// WithLLMClient sets the LLM client used by the HyDE and Judge transformers.
func WithLLMClient(client llm.Client) Option {
	return func(o *options) {
		o.llmClient = client
	}
}

// WithObservability wraps the configured LLM client (WithLLMClient) with the
// OpenTelemetry decorator so the HyDE, Judge, grounding and reranker LLM calls
// emit spans and token/latency metrics under the amoxtli instrumentation scope.
// Search latency is instrumented unconditionally. It has no effect unless an
// LLM client is set. Embeddings issued by an index built by the caller
// (sqlitevec/postgres) are not covered here — wrap that client yourself with
// llmx.NewObservableClient if you want their metrics too.
func WithObservability() Option {
	return func(o *options) {
		o.observability = true
	}
}

// WithFileConverter sets a file converter for converting files before indexing.
func WithFileConverter(fc convert.Converter) Option {
	return func(o *options) {
		o.fileConverter = fc
	}
}

// WithSourceCode enables source-code indexing for the file extensions
// registered in the registry (see sourcecode.DefaultRegistry). Source files
// are parsed with tree-sitter into declaration-level sections and tagged with
// `type=code` and `language=<name>` metadata, filterable at search time with
// WithSearchFilter.
func WithSourceCode(registry *sourcecode.Registry) Option {
	return func(o *options) {
		o.sourceCode = registry
	}
}

// WithIndexers declares the set of indexers composing the search pipeline,
// each with its relative weight. It can be called several times; indexers
// accumulate.
//
// Any implementation of index.Index can be plugged in; conformance can be
// verified with the index/testsuite package. Build the backends with their
// own constructors, e.g. bleve.OpenOrCreate(...), sqlitevec.NewIndex(...) or
// postgres.NewIndex(...), and wrap each in an Indexer.
func WithIndexers(indexers ...Indexer) Option {
	return func(o *options) {
		o.indexers = append(o.indexers, indexers...)
	}
}

// WithMaxWordsPerSection sets the maximum number of words per document section.
func WithMaxWordsPerSection(n int) Option {
	return func(o *options) {
		o.maxWordsPerSection = n
	}
}

// WithMaxTotalWords bounds the prompt size (in words) shared by the LLM
// retrieval stages: the Judge transformer, the LLM reranker and the grounding
// evidence evaluator. Words are only a coarse proxy for tokens, so keep it low
// enough that the resulting prompt fits the chat endpoint's context window.
func WithMaxTotalWords(n int) Option {
	return func(o *options) {
		o.maxTotalWords = n
	}
}

// WithTaskParallelism sets the number of concurrent tasks allowed.
func WithTaskParallelism(n int) Option {
	return func(o *options) {
		o.taskParallelism = n
	}
}

// WithDisableHyDE disables the HyDE query transformer.
func WithDisableHyDE() Option {
	return func(o *options) {
		o.disableHyDE = true
	}
}

// WithDisableJudge disables the Judge results transformer.
func WithDisableJudge() Option {
	return func(o *options) {
		o.disableJudge = true
	}
}

// WithGroundingCheck enables the fused evidence evaluator: a single LLM call
// that both relevance-filters the retrieved evidence and judges whether it
// supports a reliable answer (the grounding γ verdict). It makes CheckGrounding
// available as a standalone step and gates the re-retrieval loop of
// SearchIterative. When enabled it replaces the Judge results transformer in the
// pipeline (Search then relies on the evaluator for relevance filtering),
// avoiding a redundant LLM pass over the same evidence. Requires an LLM client
// (WithLLMClient). Disabled by default.
func WithGroundingCheck() Option {
	return func(o *options) {
		o.groundingCheck = true
	}
}

// WithGroundingFailOpen makes Search degrade gracefully when the grounding
// evidence evaluator (an LLM call) fails: instead of returning the error, Search
// logs a warning and returns the retrieved results unfiltered. Without it, a
// transient LLM failure in the evaluator fails the whole Search. Only meaningful
// together with WithGroundingCheck. Disabled by default (fail-closed).
func WithGroundingFailOpen() Option {
	return func(o *options) {
		o.groundingFailOpen = true
	}
}

// WithGroundingMinScore sets the grounding score threshold below which the
// verdict is considered not confident (default 0.4). This gates iterative
// re-retrieval only — whether SearchIterative reformulates and searches again —
// and does NOT control the relevance filtering/demotion of the evidence itself
// (that is WithGroundingMode). Only meaningful together with WithGroundingCheck.
func WithGroundingMinScore(minScore float64) Option {
	return func(o *options) {
		o.groundingMinScore = minScore
	}
}

// WithGroundingMode selects what the evaluator's relevance signal does to the
// evidence: retrieval.GroundingFilter (default) drops the sections judged
// irrelevant — maximising precision but truncating recall; retrieval.GroundingDemote
// keeps them but ranks them last — preserving recall@k while still surfacing the
// grounded evidence first. Only meaningful together with WithGroundingCheck.
func WithGroundingMode(mode retrieval.GroundingMode) Option {
	return func(o *options) {
		o.groundingMode = mode
	}
}

// WithIterativeRetrieval enables grounding-driven re-retrieval in
// SearchIterative: when the evidence is not confidently grounded the query is
// reformulated and searched again, up to rounds times (rounds <= 0 means 1).
// Requires WithGroundingCheck and an LLM client.
func WithIterativeRetrieval(rounds int) Option {
	return func(o *options) {
		o.iterativeRetrieval = true
		if rounds > 0 {
			o.iterativeMaxRounds = rounds
		}
	}
}

// WithQueryDecomposition enables splitting a complex question into at most
// maxSubQueries sub-questions in SearchIterative, searching each and fusing
// their evidence. Requires an LLM client. maxSubQueries <= 0 keeps the default
// (3).
func WithQueryDecomposition(maxSubQueries int) Option {
	return func(o *options) {
		o.queryDecomposition = true
		if maxSubQueries > 0 {
			o.decompositionMaxSubQueries = maxSubQueries
		}
	}
}

// WithReranking enables LLM-based reranking of the fused search results before
// pagination: the retrieved candidates are reordered by relevance to the query,
// reusing the WithMaxTotalWords budget to bound the prompt size. Requires an LLM
// client (WithLLMClient). Disabled by default.
func WithReranking() Option {
	return func(o *options) {
		o.reranking = true
	}
}

// WithSnapshotBoundary overrides the multipart boundary used by Backup/Restore.
func WithSnapshotBoundary(boundary string) Option {
	return func(o *options) {
		o.snapshotBoundary = boundary
	}
}

// WithIndex provides a ready-made index.Index, bypassing pipeline composition
// entirely (including the HyDE/Judge/dedup transformers). Mutually exclusive
// with WithIndexers. The caller owns and must close the index.
func WithIndex(idx index.Index) Option {
	return func(o *options) {
		o.index = idx
	}
}

// WithStore sets the document store. It is required. Build it with
// gorm.NewSQLiteStore or gorm.NewPostgresStore (or any ingest.Store). The
// caller owns and must close the store.
func WithStore(store ingest.Store) Option {
	return func(o *options) {
		o.store = store
	}
}

// WithTaskRunner provides a custom task.Runner implementation.
func WithTaskRunner(runner task.Runner) Option {
	return func(o *options) {
		o.taskRunner = runner
	}
}

// WithPersistentTasks enables a persistent, gorm-backed task runner that shares
// the document store's database. Pending indexing tasks — and tasks left
// running by a previous, interrupted process — are resumed on startup. It
// requires a gorm-backed store (gorm.NewSQLiteStore / gorm.NewPostgresStore)
// and a stable stagingDir where files awaiting indexing are kept across
// restarts (it must be a durable path, not a temporary directory). It is
// ignored when a runner is supplied explicitly with WithTaskRunner.
func WithPersistentTasks(stagingDir string) Option {
	return func(o *options) {
		o.persistentTasks = true
		o.stagingDir = stagingDir
	}
}

// WithCloseTimeout bounds how long Close waits for in-flight indexing tasks to
// drain before giving up (default 30s). A non-positive duration keeps the
// default.
func WithCloseTimeout(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.closeTimeout = d
		}
	}
}

// IndexFileOptions holds options for IndexFile calls.
type IndexFileOptions struct {
	Source      *url.URL
	ETag        string
	Collections []model.CollectionID
	// Metadata is arbitrary document metadata used for filtering at search time.
	Metadata map[string]any
}

// IndexFileOption configures an IndexFile call.
type IndexFileOption func(*IndexFileOptions)

// WithIndexFileSource sets the source URL for the indexed file.
func WithIndexFileSource(source *url.URL) IndexFileOption {
	return func(o *IndexFileOptions) {
		o.Source = source
	}
}

// WithIndexFileETag sets the ETag for the indexed file (used for deduplication).
func WithIndexFileETag(etag string) IndexFileOption {
	return func(o *IndexFileOptions) {
		o.ETag = etag
	}
}

// WithIndexFileCollections associates the indexed file with the given collection IDs.
func WithIndexFileCollections(ids ...model.CollectionID) IndexFileOption {
	return func(o *IndexFileOptions) {
		o.Collections = ids
	}
}

// WithIndexFileMetadata attaches arbitrary metadata to the indexed document,
// used for metadata filtering at search time (see WithSearchFilter).
func WithIndexFileMetadata(metadata map[string]any) IndexFileOption {
	return func(o *IndexFileOptions) {
		o.Metadata = metadata
	}
}

// SearchOptions holds options for Search calls.
type SearchOptions struct {
	// MaxResults is the page size (number of results per page).
	MaxResults  int
	Collections []model.CollectionID
	// Filter restricts results to documents whose metadata matches every
	// condition. Requires a store implementing ingest.MetadataProvider (the
	// gorm stores do).
	Filter index.Filter
	// Cursor resumes pagination after a previous SearchPage (empty = first page).
	Cursor string
}

// SearchOption configures a Search call.
type SearchOption func(*SearchOptions)

// WithSearchMaxResults sets the maximum number of search results (page size).
func WithSearchMaxResults(n int) SearchOption {
	return func(o *SearchOptions) {
		o.MaxResults = n
	}
}

// WithSearchCollections restricts the search to the given collection IDs.
func WithSearchCollections(ids ...model.CollectionID) SearchOption {
	return func(o *SearchOptions) {
		o.Collections = ids
	}
}

// WithSearchFilter restricts results to documents whose metadata satisfies the
// given filter (see index.Eq/Ne/Gt/Gte/Lt/Lte/In). Requires a store
// implementing ingest.MetadataProvider.
func WithSearchFilter(conditions ...index.Condition) SearchOption {
	return func(o *SearchOptions) {
		o.Filter = index.Filter(conditions)
	}
}

// WithSearchCursor resumes pagination after the given opaque cursor (the
// NextCursor returned by a previous SearchPage).
func WithSearchCursor(cursor string) SearchOption {
	return func(o *SearchOptions) {
		o.Cursor = cursor
	}
}
