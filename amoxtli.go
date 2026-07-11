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
	"github.com/bornholm/amoxtli/task"
	taskMemory "github.com/bornholm/amoxtli/task/memory"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// Codex is the main embedded instance: a store, index pipeline and task
// runner behind a single API.
type Codex struct {
	manager          *ingest.Manager
	index            index.Index
	store            ingest.Store
	taskRunner       task.Runner
	snapshotBoundary string
	cancel           context.CancelFunc
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
		if !opts.disableJudge && opts.llmClient != nil {
			resultsTransformers = append(resultsTransformers,
				pipeline.NewJudgeResultsTransformer(opts.llmClient, store, opts.maxTotalWords),
			)
		}
		pipelineOpts = append(pipelineOpts,
			pipeline.WithResultsTransformers(resultsTransformers...),
		)

		idx = pipeline.NewIndex(weightedIndexes, pipelineOpts...)
	}

	if taskRunner == nil {
		taskRunner = taskMemory.NewTaskRunner(
			opts.taskParallelism,
			60*time.Minute,
			10*time.Minute,
		)
	}

	managerOpts := []ingest.ManagerOptionFunc{
		ingest.WithManagerMaxWordPerSection(opts.maxWordsPerSection),
	}
	if opts.fileConverter != nil {
		managerOpts = append(managerOpts, ingest.WithManagerFileConverter(opts.fileConverter))
	}

	manager := ingest.NewManager(store, idx, taskRunner, managerOpts...)
	manager.RegisterHandlers(taskRunner)

	// Start task runner
	runnerCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	go func() {
		if err := taskRunner.Run(runnerCtx); err != nil && !errors.Is(err, context.Canceled) {
			slog.ErrorContext(runnerCtx, "amoxtli task runner stopped", slog.Any("error", err))
		}
	}()

	codex.manager = manager
	codex.index = idx
	codex.store = store
	codex.taskRunner = taskRunner
	codex.cancel = cancel

	return codex, nil
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

	taskID, err := c.manager.IndexFile(ctx, filename, r, ingestOpts...)
	if err != nil {
		return "", errors.WithStack(err)
	}

	return taskID, nil
}

// Search performs a semantic search across the indexed documents.
func (c *Codex) Search(ctx context.Context, query string, funcs ...SearchOption) ([]*index.SearchResult, error) {
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

	results, err := c.manager.Search(ctx, query, ingestOpts...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return results, nil
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

// Close stops the task runner. Resources passed in through WithStore,
// WithIndex/WithIndexers and WithTaskRunner are owned by the caller and must
// be closed by them.
func (c *Codex) Close() error {
	if c.cancel != nil {
		c.cancel()
	}

	return nil
}
