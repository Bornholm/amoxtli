package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

type ManagerOptions struct {
	MaxWordPerSection int
	FileConverter     convert.Converter
	Reranker          Reranker
	// SourceCode, when set, enables source-code indexing for the file
	// extensions registered in the registry.
	SourceCode *sourcecode.Registry
	// StagingDir, when set, pins the directory where files awaiting indexing are
	// staged to a stable location instead of a per-process temporary directory.
	// It is required for a persistent task runner: a resumed IndexFile task must
	// still find its staged file after a restart.
	StagingDir string
}

type ManagerOptionFunc func(opts *ManagerOptions)

// WithManagerStagingDir pins the ingestion staging directory to a stable
// location. Use it together with a persistent task runner so that files staged
// by IndexFile survive a restart and their resumed indexing tasks can find
// them. When set, the directory is not removed on shutdown (CleanupTempDir
// becomes a no-op).
func WithManagerStagingDir(dir string) ManagerOptionFunc {
	return func(opts *ManagerOptions) {
		opts.StagingDir = dir
	}
}

// WithManagerReranker plugs a reranker into the search pipeline: it reorders
// the fused (and filtered) candidates before pagination.
func WithManagerReranker(reranker Reranker) ManagerOptionFunc {
	return func(opts *ManagerOptions) {
		opts.Reranker = reranker
	}
}

func WithManagerFileConverter(fileConverter convert.Converter) ManagerOptionFunc {
	return func(opts *ManagerOptions) {
		opts.FileConverter = fileConverter
	}
}

func WithManagerMaxWordPerSection(maxWordPerSection int) ManagerOptionFunc {
	return func(opts *ManagerOptions) {
		opts.MaxWordPerSection = maxWordPerSection
	}
}

// WithManagerSourceCode enables source-code indexing: files whose extension is
// registered in the registry are parsed into declaration-level sections
// instead of going through the converter and markdown pipeline.
func WithManagerSourceCode(registry *sourcecode.Registry) ManagerOptionFunc {
	return func(opts *ManagerOptions) {
		opts.SourceCode = registry
	}
}

func NewManagerOptions(funcs ...ManagerOptionFunc) *ManagerOptions {
	opts := &ManagerOptions{
		MaxWordPerSection: 250,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// Manager orchestrates the ingestion pipeline: file conversion, parsing,
// storage and indexing, scheduled through a task runner.
type Manager struct {
	Store

	maxWordPerSection int
	fileConverter     convert.Converter
	sourceCode        *sourcecode.Registry
	index             index.Index
	taskRunner        task.Runner
	reranker          Reranker

	// Per-manager staging directory for files awaiting indexing (created
	// lazily by IndexFile, removed by CleanupTempDir). When stagingDirOverride
	// is set the staging directory is that stable path and is never removed on
	// shutdown, so a persistent runner can resume its indexing tasks.
	tempDirOnce        sync.Once
	tempDir            string
	tempDirErr         error
	stagingDirOverride string
}

type SearchOptions struct {
	// MaxResults is the page size (number of results returned per call).
	MaxResults int
	// Names of the collection the query will be restricted to
	Collections []model.CollectionID
	// Filter restricts results to documents whose metadata matches every
	// condition. It requires the Store to implement MetadataProvider.
	Filter index.Filter
	// Cursor resumes pagination after a previous page. Empty means the first
	// page. Use the NextCursor returned by the previous call.
	Cursor string
	// CandidatePoolSize bounds how many fused candidates are fetched before
	// filtering, reranking and pagination. 0 lets the Manager pick a sensible
	// default (the page size for plain searches, a larger pool when a filter,
	// a reranker or a cursor is in play).
	CandidatePoolSize int
}

type SearchOptionFunc func(opts *SearchOptions)

func NewSearchOptions(funcs ...SearchOptionFunc) *SearchOptions {
	opts := &SearchOptions{
		MaxResults:  5,
		Collections: make([]model.CollectionID, 0),
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

func WithSearchMaxResults(max int) SearchOptionFunc {
	return func(opts *SearchOptions) {
		opts.MaxResults = max
	}
}

func WithSearchCollections(collections ...model.CollectionID) SearchOptionFunc {
	return func(opts *SearchOptions) {
		opts.Collections = collections
	}
}

// WithSearchFilter restricts results to documents whose metadata satisfies the
// given filter. It requires the configured Store to implement MetadataProvider.
func WithSearchFilter(filter index.Filter) SearchOptionFunc {
	return func(opts *SearchOptions) {
		opts.Filter = filter
	}
}

// WithSearchCursor resumes pagination after the given opaque cursor (the
// NextCursor of a previous SearchResults).
func WithSearchCursor(cursor string) SearchOptionFunc {
	return func(opts *SearchOptions) {
		opts.Cursor = cursor
	}
}

// WithSearchCandidatePoolSize overrides the number of fused candidates fetched
// before filtering, reranking and pagination.
func WithSearchCandidatePoolSize(size int) SearchOptionFunc {
	return func(opts *SearchOptions) {
		opts.CandidatePoolSize = size
	}
}

// SearchResults holds a page of search results together with the cursor needed
// to fetch the next page (empty when the last page has been reached).
type SearchResults struct {
	Results    []*index.SearchResult
	NextCursor string
}

// defaultCandidatePool bounds the fused candidate window used when filtering,
// reranking or cursor pagination is active. Pagination is stable within this
// window; paging past it returns no further results.
const defaultCandidatePool = 100

func (m *Manager) Search(ctx context.Context, query string, funcs ...SearchOptionFunc) (*SearchResults, error) {
	opts := NewSearchOptions(funcs...)

	pageSize := opts.MaxResults
	if pageSize <= 0 {
		pageSize = 5
	}

	// Over-fetch only when filtering, reranking or paginating: a plain search
	// keeps fetching just the page size, as before.
	pool := pageSize
	switch {
	case opts.CandidatePoolSize > 0:
		pool = opts.CandidatePoolSize
	case len(opts.Filter) > 0 || m.reranker != nil || opts.Cursor != "":
		pool = max(pool, defaultCandidatePool)
	}

	collections := make([]model.CollectionID, 0)
	for _, c := range opts.Collections {
		coll, err := m.GetCollectionByID(ctx, c, false)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		collections = append(collections, coll.ID())
	}

	results, err := m.index.Search(ctx, query, index.SearchOptions{
		MaxResults:  pool,
		Collections: collections,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if len(opts.Filter) > 0 {
		results, err = m.applyMetadataFilter(ctx, results, opts.Filter)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	if m.reranker != nil {
		results, err = rerankTop(ctx, m.reranker, query, results, pageSize)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	page, nextCursor, err := paginate(results, opts.Cursor, pageSize)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return &SearchResults{Results: page, NextCursor: nextCursor}, nil
}

// rerankWindowFactor multiplies the page size to bound how many top fused
// candidates are handed to the (LLM) reranker. Reranking is only meant to order
// the head of the result set; feeding it the whole candidate pool (up to
// defaultCandidatePool) inflates the prompt — and latency — for no benefit on
// the returned page.
const rerankWindowFactor = 4

// minRerankWindow is the floor for the rerank window so small pages still give
// the reranker enough context to promote a strong-but-lower-ranked candidate.
const minRerankWindow = 20

// rerankTop reranks only the top window of results (bounded relative to the page
// size) and leaves the remaining candidates in their original fused order after
// them. This keeps deep pagination working while capping the reranker's cost.
func rerankTop(ctx context.Context, reranker Reranker, query string, results []*index.SearchResult, pageSize int) ([]*index.SearchResult, error) {
	window := max(pageSize*rerankWindowFactor, minRerankWindow)
	if len(results) <= window {
		return reranker.Rerank(ctx, query, results)
	}

	head, err := reranker.Rerank(ctx, query, results[:window])
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return append(head, results[window:]...), nil
}

// applyMetadataFilter drops results whose document metadata does not satisfy the
// filter. It requires the Store to implement MetadataProvider.
func (m *Manager) applyMetadataFilter(ctx context.Context, results []*index.SearchResult, filter index.Filter) ([]*index.SearchResult, error) {
	provider, ok := m.Store.(MetadataProvider)
	if !ok {
		return nil, errors.New("ingest: search metadata filter requires a Store implementing MetadataProvider")
	}

	sources := make([]string, 0, len(results))
	seen := map[string]struct{}{}
	for _, r := range results {
		key := sourceKey(r.Source)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		sources = append(sources, key)
	}

	metaBySource, err := provider.GetDocumentsMetadataBySources(ctx, sources)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	filtered := make([]*index.SearchResult, 0, len(results))
	for _, r := range results {
		if filter.Matches(metaBySource[sourceKey(r.Source)]) {
			filtered = append(filtered, r)
		}
	}

	return filtered, nil
}

// sourceKey normalizes a result source to the key under which its document is
// stored (fragments identify sections, not documents).
func sourceKey(source *url.URL) string {
	if source == nil {
		return ""
	}
	if source.Fragment == "" {
		return source.String()
	}
	clone := *source
	clone.Fragment = ""
	return clone.String()
}

// searchCursor is the opaque pagination anchor. Each fused result maps to a
// unique source, so the source string uniquely identifies the last returned
// result; the score is carried for diagnostics.
type searchCursor struct {
	Source string  `json:"s"`
	Score  float64 `json:"c"`
}

func encodeCursor(r *index.SearchResult) (string, error) {
	data, err := json.Marshal(searchCursor{Source: r.Source.String(), Score: r.Score})
	if err != nil {
		return "", errors.WithStack(err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCursor(cursor string) (searchCursor, error) {
	var c searchCursor
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return c, errors.Wrap(err, "invalid search cursor")
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, errors.Wrap(err, "invalid search cursor")
	}
	return c, nil
}

// paginate returns the page of at most pageSize results following the cursor,
// plus the cursor for the next page (empty when exhausted). Because each result
// has a unique source, the cursor anchors on the last returned source; if that
// anchor is no longer present (the underlying data changed) pagination stops.
func paginate(results []*index.SearchResult, cursor string, pageSize int) ([]*index.SearchResult, string, error) {
	start := 0
	if cursor != "" {
		anchor, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", errors.WithStack(err)
		}
		start = len(results)
		for i, r := range results {
			if r.Source.String() == anchor.Source {
				start = i + 1
				break
			}
		}
	}

	if start >= len(results) {
		return []*index.SearchResult{}, "", nil
	}

	end := start + pageSize
	if end > len(results) {
		end = len(results)
	}

	page := results[start:end]

	nextCursor := ""
	if end < len(results) && len(page) > 0 {
		var err error
		nextCursor, err = encodeCursor(page[len(page)-1])
		if err != nil {
			return nil, "", errors.WithStack(err)
		}
	}

	return page, nextCursor, nil
}

func (m *Manager) SupportedExtensions() []string {
	exts := []string{".md"}

	if m.fileConverter != nil {
		exts = append(exts, m.fileConverter.SupportedExtensions()...)
	}

	if m.sourceCode != nil {
		exts = append(exts, m.sourceCode.SupportedExtensions()...)
	}

	slices.Sort(exts)

	return slices.Compact(exts)
}

type IndexFileOptions struct {
	ETag   string
	Source *url.URL
	// Names of the collection to associate with the document
	Collections []model.CollectionID
	// Arbitrary document metadata used for filtering at search time.
	Metadata map[string]any
}

type IndexFileOptionFunc func(opts *IndexFileOptions)

// WithIndexFileMetadata attaches arbitrary metadata to the indexed document,
// used for metadata filtering at search time.
func WithIndexFileMetadata(metadata map[string]any) IndexFileOptionFunc {
	return func(opts *IndexFileOptions) {
		opts.Metadata = metadata
	}
}

func WithIndexFileCollections(collections ...model.CollectionID) IndexFileOptionFunc {
	return func(opts *IndexFileOptions) {
		opts.Collections = collections
	}
}

func WithIndexFileSource(source *url.URL) IndexFileOptionFunc {
	return func(opts *IndexFileOptions) {
		opts.Source = source
	}
}

func WithIndexFileETag(etag string) IndexFileOptionFunc {
	return func(opts *IndexFileOptions) {
		opts.ETag = etag
	}
}

func NewIndexFileOptions(funcs ...IndexFileOptionFunc) *IndexFileOptions {
	opts := &IndexFileOptions{}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// IndexFile copies the file to a temporary location then schedules an
// asynchronous indexing task. The returned task.ID can be used to track
// progress through the task runner.
func (m *Manager) IndexFile(ctx context.Context, filename string, r io.Reader, funcs ...IndexFileOptionFunc) (task.ID, error) {
	opts := NewIndexFileOptions(funcs...)

	tempDir, err := m.stagingDir()
	if err != nil {
		return "", errors.WithStack(err)
	}

	ext := filepath.Ext(filename)
	path := filepath.Join(tempDir, xid.New().String()+ext)

	file, err := os.Create(path)
	if err != nil {
		return "", errors.WithStack(err)
	}

	if _, err := io.Copy(file, r); err != nil {
		return "", errors.WithStack(err)
	}

	indexFileTask := NewIndexFileTask(path, filename, opts.ETag, opts.Source, opts.Collections, opts.Metadata)

	taskCtx := slogx.WithAttrs(context.Background(), slog.String("filename", filename), slog.String("filepath", path))

	if err := m.taskRunner.ScheduleTask(taskCtx, indexFileTask); err != nil {
		return "", errors.WithStack(err)
	}

	return indexFileTask.ID(), nil
}

func (m *Manager) CleanupIndex(ctx context.Context, collections ...model.CollectionID) (task.ID, error) {
	cleanupIndexTask := NewCleanupTask(collections)

	if err := m.taskRunner.ScheduleTask(ctx, cleanupIndexTask); err != nil {
		return "", errors.WithStack(err)
	}

	return cleanupIndexTask.ID(), nil
}

// ReindexCollection rebuilds the index for a single collection.
func (m *Manager) ReindexCollection(ctx context.Context, collectionID model.CollectionID) (task.ID, error) {
	reindexTask := NewReindexTask(collectionID)

	if err := m.taskRunner.ScheduleTask(ctx, reindexTask); err != nil {
		return "", errors.WithStack(err)
	}

	return reindexTask.ID(), nil
}

// Reindex rebuilds the index for the whole store.
func (m *Manager) Reindex(ctx context.Context) (task.ID, error) {
	return m.ReindexCollection(ctx, "")
}

// RegisterHandlers registers the ingestion task handlers on the given runner.
// When the runner is a task.PersistentRunner, the matching deserialization
// factories are registered too so pending tasks can be rebuilt and resumed
// after a restart.
func (m *Manager) RegisterHandlers(runner task.Runner) {
	runner.RegisterTask(TaskTypeIndexFile, NewIndexFileHandler(m.Store, m.fileConverter, m.index, m.maxWordPerSection, WithIndexFileHandlerSourceCode(m.sourceCode)))
	runner.RegisterTask(TaskTypeCleanup, NewCleanupHandler(m.index, m.Store))
	runner.RegisterTask(TaskTypeReindex, NewReindexHandler(m.Store, m.index, m.maxWordPerSection))

	if persistent, ok := runner.(task.PersistentRunner); ok {
		persistent.RegisterFactory(TaskTypeIndexFile, IndexFileTaskFactory)
		persistent.RegisterFactory(TaskTypeCleanup, CleanupTaskFactory)
		persistent.RegisterFactory(TaskTypeReindex, ReindexTaskFactory)
	}
}

func NewManager(store Store, idx index.Index, taskRunner task.Runner, funcs ...ManagerOptionFunc) *Manager {
	opts := NewManagerOptions(funcs...)

	// Best-effort: sweep staging directories left behind by previous runs that
	// crashed before their indexing tasks removed the staged files. Only dirs
	// older than staleTempDirAge are removed, so a concurrently-running
	// instance's fresh directory is never touched. Skipped when a stable staging
	// directory is configured (its files may back pending, resumable tasks).
	if opts.StagingDir == "" {
		cleanStaleTempDirs(staleTempDirAge)
	}

	manager := &Manager{
		maxWordPerSection:  opts.MaxWordPerSection,
		Store:              store,
		taskRunner:         taskRunner,
		index:              idx,
		fileConverter:      opts.FileConverter,
		sourceCode:         opts.SourceCode,
		reranker:           opts.Reranker,
		stagingDirOverride: opts.StagingDir,
	}

	return manager
}

// tempDirPrefix names this library's staging directories under os.TempDir().
const tempDirPrefix = "amoxtli-"

// staleTempDirAge is how old a staging directory must be before the startup
// sweep considers it orphaned.
const staleTempDirAge = 24 * time.Hour

// stagingDir returns the per-manager staging directory, creating it on first
// use. When a stable staging directory is configured (WithManagerStagingDir) it
// is used as-is (created if missing) instead of a per-process temporary
// directory.
func (m *Manager) stagingDir() (string, error) {
	m.tempDirOnce.Do(func() {
		if m.stagingDirOverride != "" {
			if err := os.MkdirAll(m.stagingDirOverride, 0o700); err != nil {
				m.tempDirErr = errors.WithStack(err)
				return
			}
			m.tempDir = m.stagingDirOverride
			return
		}
		tmp, err := os.MkdirTemp("", tempDirPrefix+"*")
		if err != nil {
			m.tempDirErr = errors.WithStack(err)
			return
		}
		m.tempDir = tmp
	})
	if m.tempDirErr != nil {
		return "", errors.WithStack(m.tempDirErr)
	}
	return m.tempDir, nil
}

// CleanupTempDir removes this manager's staging directory and everything left
// in it. It is a best-effort, idempotent operation typically called on
// shutdown, once in-flight indexing tasks have drained. It is a no-op when a
// stable staging directory is configured, since its files may back pending,
// resumable tasks that must survive the restart.
func (m *Manager) CleanupTempDir() error {
	if m.stagingDirOverride != "" {
		return nil
	}
	if m.tempDir == "" {
		return nil
	}
	if err := os.RemoveAll(m.tempDir); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// cleanStaleTempDirs removes staging directories under os.TempDir() whose
// modification time is older than maxAge. Errors are logged and ignored so a
// cleanup failure never prevents startup.
func cleanStaleTempDirs(maxAge time.Duration) {
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), tempDirPrefix+"*"))
	if err != nil {
		slog.Warn("could not list stale staging directories", slog.Any("error", errors.WithStack(err)))
		return
	}

	cutoff := time.Now().Add(-maxAge)
	for _, dir := range matches {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			slog.Warn("could not remove stale staging directory", slog.String("dir", dir), slog.Any("error", errors.WithStack(err)))
			continue
		}
		slog.Debug("removed stale staging directory", slog.String("dir", dir))
	}
}
