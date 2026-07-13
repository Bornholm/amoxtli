package amoxtli

import (
	"net/url"
	"time"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
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
	iterativeRetrieval         bool
	iterativeMaxRounds         int
	queryDecomposition         bool
	decompositionMaxSubQueries int
	// Indexers composing the search pipeline.
	indexers []Indexer
	// Explicit components.
	index      index.Index
	store      ingest.Store
	taskRunner task.Runner
	// closeTimeout bounds how long Close waits for in-flight tasks to drain.
	closeTimeout time.Duration
}

func defaultOptions() *options {
	return &options{
		maxWordsPerSection:         250,
		maxTotalWords:              50000,
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

// WithFileConverter sets a file converter for converting files before indexing.
func WithFileConverter(fc convert.Converter) Option {
	return func(o *options) {
		o.fileConverter = fc
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

// WithMaxTotalWords sets the maximum total words used by the Judge transformer.
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
// verdict is considered not confident (default 0.4). Only meaningful together
// with WithGroundingCheck.
func WithGroundingMinScore(minScore float64) Option {
	return func(o *options) {
		o.groundingMinScore = minScore
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

// SearchOptions holds options for Search calls.
type SearchOptions struct {
	MaxResults  int
	Collections []model.CollectionID
}

// SearchOption configures a Search call.
type SearchOption func(*SearchOptions)

// WithSearchMaxResults sets the maximum number of search results.
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
