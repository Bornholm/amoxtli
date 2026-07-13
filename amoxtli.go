// Package amoxtli provides a multi-backend document indexing and
// file-ingestion library: full-text search (bleve), vector search
// (sqlite-vec), weighted result merging, markdown chunking, file
// conversion and snapshot/restore of indexes.
package amoxtli

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"time"

	"github.com/bornholm/amoxtli/backup"
	"github.com/bornholm/amoxtli/index/pipeline"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/amoxtli/task"
	taskGorm "github.com/bornholm/amoxtli/task/gorm"
	taskMemory "github.com/bornholm/amoxtli/task/memory"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// gormDBProvider is satisfied by the gorm-backed store, exposing the shared
// *gorm.DB used to build the persistent task runner (WithPersistentTasks).
type gormDBProvider interface {
	DB() *gorm.DB
}

// Codex is the main embedded instance: a store, index pipeline and task
// runner behind a single API.
type Codex struct {
	manager           *ingest.Manager
	index             index.Index
	store             ingest.Store
	taskRunner        task.Runner
	evaluator         retrieval.EvidenceEvaluator
	groundingFailOpen bool
	orchestrator      *retrieval.Orchestrator
	snapshotBoundary  string
	cancel            context.CancelFunc
	runnerDone        chan struct{}
	closeTimeout      time.Duration
}

// New creates a new embedded Codex instance.
//
// A store (WithStore) and an index (WithIndex or WithIndexers) are required;
// build them with the dedicated constructors, e.g. gorm.NewSQLiteStore /
// gorm.NewPostgresStore for the store and bleve.OpenOrCreate /
// sqlitevec.NewIndex / postgres.NewIndex for the indexers. The caller owns
// the resources it constructs and must Close them; Codex.Close only stops the
// task runner.
func New(ctx context.Context, funcs ...Option) (*Codex, error) {
	opts := defaultOptions()
	for _, fn := range funcs {
		fn(opts)
	}

	codex := &Codex{
		snapshotBoundary: opts.snapshotBoundary,
	}

	store := opts.store
	if store == nil {
		return nil, errors.New("amoxtli: WithStore is required")
	}

	idx := opts.index
	taskRunner := opts.taskRunner

	if idx == nil {
		if len(opts.indexers) == 0 {
			return nil, errors.New("amoxtli: WithIndex or WithIndexers is required")
		}

		weightedIndexes := pipeline.WeightedIndexes{}
		for _, indexer := range opts.indexers {
			weightedIndexes[pipeline.NewIdentifiedIndex(indexer.ID, indexer.Index)] = indexer.Weight
		}

		pipelineOpts := []pipeline.OptionFunc{}
		if !opts.disableHyDE && opts.llmClient != nil {
			if lister, ok := store.(pipeline.CollectionLister); ok {
				pipelineOpts = append(pipelineOpts,
					pipeline.WithQueryTransformers(
						pipeline.NewHyDEQueryTransformer(opts.llmClient, lister),
					),
				)
			}
		}

		resultsTransformers := []pipeline.ResultsTransformer{
			pipeline.NewDuplicateContentResultsTransformer(store),
		}
		// When grounding is enabled the fused EvidenceEvaluator takes over
		// relevance filtering (applied by Search and the orchestrator), so the
		// Judge transformer is left out of the pipeline to avoid a redundant LLM
		// pass over the same evidence.
		if !opts.disableJudge && opts.llmClient != nil && !opts.groundingCheck {
			resultsTransformers = append(resultsTransformers,
				pipeline.NewJudgeResultsTransformer(opts.llmClient, store, opts.maxTotalWords),
			)
		}
		pipelineOpts = append(pipelineOpts,
			pipeline.WithResultsTransformers(resultsTransformers...),
		)

		idx = pipeline.NewIndex(weightedIndexes, pipelineOpts...)
	}

	managerOpts := []ingest.ManagerOptionFunc{
		ingest.WithManagerMaxWordPerSection(opts.maxWordsPerSection),
	}

	if taskRunner == nil {
		if opts.persistentTasks {
			if opts.stagingDir == "" {
				return nil, errors.New("amoxtli: WithPersistentTasks requires a non-empty staging directory")
			}

			provider, ok := store.(gormDBProvider)
			if !ok {
				return nil, errors.New("amoxtli: WithPersistentTasks requires a gorm-backed store (gorm.NewSQLiteStore / gorm.NewPostgresStore)")
			}

			taskRunner = taskGorm.NewTaskRunner(
				provider.DB(),
				opts.taskParallelism,
				60*time.Minute,
				10*time.Minute,
			)

			// A resumed IndexFile task must find its staged file after a restart,
			// so the staging directory must be stable rather than per-process.
			managerOpts = append(managerOpts, ingest.WithManagerStagingDir(opts.stagingDir))
		} else {
			taskRunner = taskMemory.NewTaskRunner(
				opts.taskParallelism,
				60*time.Minute,
				10*time.Minute,
			)
		}
	}

	if opts.fileConverter != nil {
		managerOpts = append(managerOpts, ingest.WithManagerFileConverter(opts.fileConverter))
	}
	if opts.reranking && opts.llmClient != nil {
		managerOpts = append(managerOpts,
			ingest.WithManagerReranker(retrieval.NewLLMReranker(opts.llmClient, store, opts.maxTotalWords)),
		)
	}

	manager := ingest.NewManager(store, idx, taskRunner, managerOpts...)
	manager.RegisterHandlers(taskRunner)

	// Start task runner. runnerDone is closed once Run returns, which the
	// memory runner does only after in-flight tasks have drained — Close waits
	// on it (bounded by closeTimeout) for a graceful shutdown.
	runnerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		if err := taskRunner.Run(runnerCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(runnerCtx, "amoxtli task runner stopped", slog.Any("error", err))
		}
	}()

	codex.manager = manager
	codex.index = idx
	codex.store = store
	codex.taskRunner = taskRunner

	// The fused evidence evaluator (relevance filtering + grounding verdict) is
	// shared across Search (filtering), CheckGrounding (verdict) and the
	// orchestrator/SearchIterative (both). It replaces the pipeline Judge when
	// enabled.
	if opts.groundingCheck && opts.llmClient != nil {
		codex.evaluator = retrieval.NewLLMEvidenceEvaluator(opts.llmClient, store, opts.maxTotalWords)
	}
	codex.groundingFailOpen = opts.groundingFailOpen
	codex.orchestrator = newOrchestrator(opts, manager, codex.evaluator)

	codex.cancel = cancel
	codex.runnerDone = runnerDone
	codex.closeTimeout = opts.closeTimeout

	return codex, nil
}

// newOrchestrator builds the explicit re-retrieval orchestrator (used by
// SearchIterative) wired to the ingest manager's Search. When evaluator is nil
// and no decomposition/reformulation is enabled it degrades to a plain search.
func newOrchestrator(opts *options, manager *ingest.Manager, evaluator retrieval.EvidenceEvaluator) *retrieval.Orchestrator {
	searchFn := func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
		ingestOpts := []ingest.SearchOptionFunc{
			ingest.WithSearchMaxResults(maxResults),
		}
		if len(collections) > 0 {
			ingestOpts = append(ingestOpts, ingest.WithSearchCollections(collections...))
		}
		res, err := manager.Search(ctx, query, ingestOpts...)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return res.Results, nil
	}

	orchestratorOpts := []retrieval.OrchestratorOption{}
	if evaluator != nil {
		orchestratorOpts = append(orchestratorOpts,
			retrieval.WithEvidenceEvaluator(evaluator),
			retrieval.WithGroundingMinScore(opts.groundingMinScore),
		)
	}
	if opts.iterativeRetrieval && opts.llmClient != nil {
		orchestratorOpts = append(orchestratorOpts,
			retrieval.WithQueryReformulator(retrieval.NewLLMQueryReformulator(opts.llmClient)),
			retrieval.WithMaxRounds(opts.iterativeMaxRounds),
		)
	}
	if opts.queryDecomposition && opts.llmClient != nil {
		orchestratorOpts = append(orchestratorOpts,
			retrieval.WithQueryDecomposer(
				retrieval.NewLLMQueryDecomposer(opts.llmClient, opts.decompositionMaxSubQueries),
			),
		)
	}

	return retrieval.NewOrchestrator(searchFn, orchestratorOpts...)
}

// IndexFile indexes a file into the given collection.
// Returns a task.ID that can be used to track progress via TaskState.
func (c *Codex) IndexFile(ctx context.Context, collectionID model.CollectionID, filename string, r io.Reader, funcs ...IndexFileOption) (task.ID, error) {
	opts := &IndexFileOptions{
		Collections: []model.CollectionID{collectionID},
	}
	for _, fn := range funcs {
		fn(opts)
	}

	ingestOpts := []ingest.IndexFileOptionFunc{
		ingest.WithIndexFileCollections(opts.Collections...),
	}
	if opts.Source != nil {
		ingestOpts = append(ingestOpts, ingest.WithIndexFileSource(opts.Source))
	}
	if opts.ETag != "" {
		ingestOpts = append(ingestOpts, ingest.WithIndexFileETag(opts.ETag))
	}
	if len(opts.Metadata) > 0 {
		ingestOpts = append(ingestOpts, ingest.WithIndexFileMetadata(opts.Metadata))
	}

	taskID, err := c.manager.IndexFile(ctx, filename, r, ingestOpts...)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return taskID, nil
}

// searchCandidatePool is the fused candidate window fetched by SearchPage so
// that cursor pagination works from the first page onward.
const searchCandidatePool = 100

// SearchPage is a page of search results together with the opaque cursor used
// to fetch the next page (empty when the last page has been reached).
type SearchPage struct {
	Results    []*index.SearchResult
	NextCursor string
}

// Search performs a semantic search across the indexed documents. It returns a
// single page of results (metadata filtering via WithSearchFilter and reranking
// via WithReranking still apply); use SearchPage when the pagination cursor is
// needed.
func (c *Codex) Search(ctx context.Context, query string, funcs ...SearchOption) ([]*index.SearchResult, error) {
	// candidatePool 0: let the Manager size the pool (cheap for plain searches,
	// larger automatically when a filter/reranker is active).
	page, err := c.searchPage(ctx, query, 0, funcs...)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return page.Results, nil
}

// SearchPage performs a search and returns a page of results plus the cursor to
// resume from (WithSearchCursor). It supports metadata filtering
// (WithSearchFilter), reranking (WithReranking) and cursor pagination. Unlike
// Search it always over-fetches a bounded candidate window so that a next
// cursor can be produced from the first page.
func (c *Codex) SearchPage(ctx context.Context, query string, funcs ...SearchOption) (*SearchPage, error) {
	return c.searchPage(ctx, query, searchCandidatePool, funcs...)
}

func (c *Codex) searchPage(ctx context.Context, query string, candidatePool int, funcs ...SearchOption) (*SearchPage, error) {
	opts := &SearchOptions{
		MaxResults: 5,
	}
	for _, fn := range funcs {
		fn(opts)
	}

	ingestOpts := []ingest.SearchOptionFunc{
		ingest.WithSearchMaxResults(opts.MaxResults),
	}
	if len(opts.Collections) > 0 {
		ingestOpts = append(ingestOpts, ingest.WithSearchCollections(opts.Collections...))
	}
	if len(opts.Filter) > 0 {
		ingestOpts = append(ingestOpts, ingest.WithSearchFilter(opts.Filter))
	}
	if opts.Cursor != "" {
		ingestOpts = append(ingestOpts, ingest.WithSearchCursor(opts.Cursor))
	}
	if candidatePool > 0 {
		ingestOpts = append(ingestOpts, ingest.WithSearchCandidatePoolSize(candidatePool))
	}

	res, err := c.manager.Search(ctx, query, ingestOpts...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	results := res.Results

	// When grounding is enabled the Judge is out of the pipeline; the evidence
	// evaluator provides the relevance filtering here instead (its verdict is
	// computed but not surfaced by Search — use CheckGrounding for that).
	if c.evaluator != nil {
		evaluation, err := c.evaluator.Evaluate(ctx, query, results)
		if err != nil {
			// Fail-open: a transient evaluator (LLM) failure returns the
			// retrieved results unfiltered rather than failing the whole Search.
			if c.groundingFailOpen {
				slog.WarnContext(ctx, "amoxtli: grounding evaluation failed, returning unfiltered results (fail-open)", slog.Any("error", errors.WithStack(err)))
				return &SearchPage{Results: results, NextCursor: res.NextCursor}, nil
			}
			return nil, errors.WithStack(err)
		}
		results = retrieval.FilterRelevant(results, evaluation.Relevant)
	}

	return &SearchPage{Results: results, NextCursor: res.NextCursor}, nil
}

// CheckGrounding evaluates whether the given search results support a reliable,
// grounded answer to the query, returning a verdict (status + score +
// explanation). It is a standalone step, fully decoupled from Search: run Search
// first, then pass its results here to decide whether to trust them or abstain.
// Requires WithGroundingCheck and an LLM client; otherwise it returns an error.
func (c *Codex) CheckGrounding(ctx context.Context, query string, results []*index.SearchResult) (*retrieval.GroundingResult, error) {
	if c.evaluator == nil {
		return nil, errors.New("amoxtli: grounding not configured (use WithLLMClient and WithGroundingCheck)")
	}

	evaluation, err := c.evaluator.Evaluate(ctx, query, results)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	result := &evaluation.Grounding

	return result, nil
}

// SearchIterative runs the explicit re-retrieval orchestration on top of Search:
// optional query decomposition and, when a grounding checker and reformulator
// are configured, grounding-gated iterative re-retrieval. It returns the fused
// evidence together with the final grounding verdict (retrieval.Result). It is a
// separate entry point from Search (which stays a plain, single-shot retrieval)
// and from CheckGrounding; with none of the orchestration options enabled it is
// equivalent to Search.
func (c *Codex) SearchIterative(ctx context.Context, query string, funcs ...SearchOption) (*retrieval.Result, error) {
	opts := &SearchOptions{
		MaxResults: 5,
	}
	for _, fn := range funcs {
		fn(opts)
	}

	result, err := c.orchestrator.Search(ctx, query, opts.MaxResults, opts.Collections)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return result, nil
}

// GetSectionsByIDs returns the sections matching the given IDs, typically
// used to retrieve the content behind search results.
func (c *Codex) GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error) {
	sections, err := c.store.GetSectionsByIDs(ctx, ids)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return sections, nil
}

// TaskState returns the current state of an indexing task.
func (c *Codex) TaskState(ctx context.Context, id task.ID) (*task.State, error) {
	state, err := c.taskRunner.GetTaskState(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return state, nil
}

// CreateCollection creates a new collection and returns its ID.
func (c *Codex) CreateCollection(ctx context.Context, label string) (model.CollectionID, error) {
	coll, err := c.store.CreateCollection(ctx, label)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return coll.ID(), nil
}

// DeleteBySource removes all documents and index entries for the given source URL.
func (c *Codex) DeleteBySource(ctx context.Context, source *url.URL) error {
	if err := c.index.DeleteBySource(ctx, source); err != nil {
		return errors.WithStack(err)
	}

	if err := c.store.DeleteDocumentBySource(ctx, source); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// CleanupIndex schedules a cleanup of orphaned documents and obsolete index entries.
func (c *Codex) CleanupIndex(ctx context.Context, collections ...model.CollectionID) (task.ID, error) {
	taskID, err := c.manager.CleanupIndex(ctx, collections...)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return taskID, nil
}

// ReindexCollection schedules a rebuild of the index for a single collection.
func (c *Codex) ReindexCollection(ctx context.Context, collectionID model.CollectionID) (task.ID, error) {
	taskID, err := c.manager.ReindexCollection(ctx, collectionID)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return taskID, nil
}

// Reindex schedules a rebuild of the whole index.
func (c *Codex) Reindex(ctx context.Context) (task.ID, error) {
	taskID, err := c.manager.Reindex(ctx)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return taskID, nil
}

// Backup streams a snapshot of the index and the document store as a
// multipart archive.
func (c *Codex) Backup(ctx context.Context) (io.ReadCloser, error) {
	reader, err := c.compositeSnapshotable().GenerateSnapshot(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return reader, nil
}

// Restore synchronously restores a snapshot previously produced by Backup.
func (c *Codex) Restore(ctx context.Context, r io.Reader) error {
	if err := c.compositeSnapshotable().RestoreSnapshot(ctx, r); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (c *Codex) compositeSnapshotable() *backup.Composite {
	snapshotables := make([]backup.IdentifiedSnapshotable, 0)

	if snapshotableIndex, ok := c.index.(backup.Snapshotable); ok {
		snapshotables = append(snapshotables, backup.WithSnapshotID("index-v1", snapshotableIndex))
	}

	if snapshotableStore, ok := c.store.(backup.Snapshotable); ok {
		snapshotables = append(snapshotables, backup.WithSnapshotID("store-v1", snapshotableStore))
	}

	return backup.ComposeSnapshots(c.snapshotBoundary, snapshotables...)
}

// Manager returns the underlying ingest.Manager for advanced usage.
func (c *Codex) Manager() *ingest.Manager {
	return c.manager
}

// Index returns the underlying index.Index for advanced usage.
func (c *Codex) Index() index.Index {
	return c.index
}

// Close stops the task runner, waiting up to the configured close timeout
// (WithCloseTimeout, default 30s) for in-flight indexing tasks to drain, then
// removes the ingestion staging directory. Resources passed in through
// WithStore, WithIndex/WithIndexers and WithTaskRunner are owned by the caller
// and must be closed by them.
func (c *Codex) Close() error {
	if c.cancel != nil {
		c.cancel()
	}

	// Wait for the task runner to drain in-flight tasks, bounded by the timeout.
	if c.runnerDone != nil {
		timeout := c.closeTimeout
		if timeout <= 0 {
			timeout = 30 * time.Second
		}

		timer := time.NewTimer(timeout)
		defer timer.Stop()

		select {
		case <-c.runnerDone:
		case <-timer.C:
			slog.Warn("amoxtli: task runner drain timed out on Close", slog.Duration("timeout", timeout))
		}
	}

	// Best-effort staging directory cleanup: after a graceful drain no task
	// still needs its staged file.
	if c.manager != nil {
		if err := c.manager.CleanupTempDir(); err != nil {
			slog.Warn("amoxtli: could not remove staging directory on Close", slog.Any("error", errors.WithStack(err)))
		}
	}

	return nil
}
