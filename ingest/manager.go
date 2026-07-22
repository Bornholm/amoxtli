package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// CandidatePoolSize pins how many fused candidates are fetched before
	// filtering, reranking and pagination, disabling the adaptive sizing. 0
	// (the default) lets the Manager size the window from the requested page,
	// widening it while a filter leaves too few survivors.
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

// defaultCandidatePool is the fused candidate window used when reranking or
// cursor pagination is active without a metadata filter.
const defaultCandidatePool = 100

const (
	// candidateFetchFactor over-fetches relative to the number of results the
	// requested page actually needs, so that a moderately selective filter
	// still yields a full page in a single round-trip.
	candidateFetchFactor = 3

	// candidateFetchGrowth multiplies the window on each round that did not
	// gather enough surviving results.
	candidateFetchGrowth = 3

	// maxCandidateFetch hard-bounds the over-fetch. Past it, recall under a very
	// selective filter is degraded rather than unbounded: a filter matching one
	// document in a thousand may return a short page even though more documents
	// would match. This is the price of filtering outside the index; backends
	// implementing index.FilterableIndex will not pay it.
	maxCandidateFetch = 500
)

// candidateWindow returns the number of fused candidates to fetch in order to
// serve `target` results after filtering.
//
// It depends only on target — itself derived from the cursor's offset plus the
// page size — never on state carried between calls. Replaying the same page
// therefore fetches the same window and yields the same ordering, which is what
// keeps pagination stable across a multi-instance deployment.
func candidateWindow(target, factor int) int {
	if target <= 0 {
		return 0
	}

	// Guard against a forged or absurd cursor offset overflowing the product.
	if target > maxCandidateFetch {
		return maxCandidateFetch
	}

	return min(target*factor, maxCandidateFetch)
}

func (m *Manager) Search(ctx context.Context, query string, funcs ...SearchOptionFunc) (*SearchResults, error) {
	opts := NewSearchOptions(funcs...)

	// Filter keys reach us from caller-facing surfaces (CLI flags, MCP tool
	// arguments): reject the invalid ones here, before they are evaluated or
	// translated into a backend query.
	if err := opts.Filter.Validate(); err != nil {
		return nil, errors.WithStack(err)
	}

	pageSize := opts.MaxResults
	if pageSize <= 0 {
		pageSize = 5
	}

	// The number of results the requested page needs: everything skipped by the
	// cursor, plus the page itself, plus one more — without that extra result
	// there is no way to tell a full last page from a page followed by others,
	// and pagination would stop one page early.
	fingerprint := filterFingerprint(opts.Filter)

	resumeOffset, err := resumeCursor(opts.Cursor, fingerprint)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	target := resumeOffset + pageSize + 1

	collections := make([]model.CollectionID, 0)
	for _, c := range opts.Collections {
		coll, err := m.GetCollectionByID(ctx, c, false)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		collections = append(collections, coll.ID())
	}

	results, err := m.searchCandidates(ctx, query, collections, opts, target)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if m.reranker != nil {
		results, err = rerankTop(ctx, m.reranker, query, results, pageSize)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	page, nextCursor, err := paginate(results, opts.Cursor, pageSize, fingerprint)
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

// searchCandidates fetches the fused candidates backing the requested page,
// applying the metadata filter when there is one.
//
// Filtering happens outside the index, so asking for `target` candidates may
// leave fewer than `target` once the filter has run. The window therefore grows
// until enough results survive, the backend runs out of candidates, or the hard
// bound is reached. The growth is deterministic — it depends only on target —
// so replaying a page fetches the same window and returns the same ordering.
func (m *Manager) searchCandidates(ctx context.Context, query string, collections []model.CollectionID, opts *SearchOptions, target int) ([]*index.SearchResult, error) {
	// When the index applies the filter inside its own query, its top-k is
	// already k matching results: no over-fetching, no metadata reload, no
	// second round. The contract of index.FilterableIndex — enforced by the
	// shared conformance suite — is what allows skipping the Go filter entirely
	// rather than re-checking its work.
	if filterable, ok := index.AsFilterable(m.index); ok && len(opts.Filter) > 0 {
		limit := target
		if opts.CandidatePoolSize > 0 {
			limit = opts.CandidatePoolSize
		}

		results, err := filterable.SearchFiltered(ctx, query, opts.Filter, index.SearchOptions{
			MaxResults:  min(limit, maxCandidateFetch),
			Collections: collections,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return results, nil
	}

	search := func(limit int) ([]*index.SearchResult, error) {
		results, err := m.index.Search(ctx, query, index.SearchOptions{
			MaxResults:  limit,
			Collections: collections,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return results, nil
	}

	// An explicit pool size is an override: honour it verbatim, no adaptation.
	if opts.CandidatePoolSize > 0 {
		results, err := search(opts.CandidatePoolSize)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if len(opts.Filter) == 0 {
			return results, nil
		}

		filter, err := m.newMetadataFilter(opts.Filter)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return filter.apply(ctx, results)
	}

	// Without a filter nothing is dropped after the fact, so one round always
	// suffices; the wider window only serves reranking and cursor pagination.
	if len(opts.Filter) == 0 {
		window := target
		if m.reranker != nil || target > 1 {
			window = max(window, min(defaultCandidatePool, maxCandidateFetch))
		}

		return search(min(window, maxCandidateFetch))
	}

	filter, err := m.newMetadataFilter(opts.Filter)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	fetch := candidateWindow(target, candidateFetchFactor)

	for {
		results, err := search(fetch)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		kept, err := filter.apply(ctx, results)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		// Enough survivors, backend exhausted, or window maxed out: this is as
		// good as it gets.
		if len(kept) >= target || len(results) < fetch || fetch >= maxCandidateFetch {
			return kept, nil
		}

		fetch = min(fetch*candidateFetchGrowth, maxCandidateFetch)
	}
}

// metadataFilter evaluates a metadata filter against search results, caching
// each document's verdict for the lifetime of one search.
//
// The cache matters because the over-fetch loop re-queries the index with a
// wider window: without it, every round would reload the metadata of the
// documents already judged in the previous one.
type metadataFilter struct {
	provider MetadataProvider
	filter   index.Filter
	verdicts map[string]bool
}

// newMetadataFilter fails when the Store cannot supply document metadata, which
// is a configuration error rather than an empty result set.
func (m *Manager) newMetadataFilter(filter index.Filter) (*metadataFilter, error) {
	provider, ok := m.Store.(MetadataProvider)
	if !ok {
		return nil, errors.New("ingest: search metadata filter requires a Store implementing MetadataProvider")
	}

	return &metadataFilter{
		provider: provider,
		filter:   filter,
		verdicts: map[string]bool{},
	}, nil
}

// apply returns the results whose document satisfies the filter, in order. The
// metadata of every not-yet-judged document is loaded in a single batch query.
func (f *metadataFilter) apply(ctx context.Context, results []*index.SearchResult) ([]*index.SearchResult, error) {
	unjudged := make([]string, 0, len(results))
	for _, r := range results {
		key := sourceKey(r.Source)
		if _, known := f.verdicts[key]; known {
			continue
		}

		// Mark it now so that several sections of the same document do not
		// queue their source twice.
		f.verdicts[key] = false
		unjudged = append(unjudged, key)
	}

	if len(unjudged) > 0 {
		metaBySource, err := f.provider.GetDocumentsMetadataBySources(ctx, unjudged)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		for _, key := range unjudged {
			f.verdicts[key] = f.filter.Matches(metaBySource[key])
		}
	}

	kept := make([]*index.SearchResult, 0, len(results))
	for _, r := range results {
		if f.verdicts[sourceKey(r.Source)] {
			kept = append(kept, r)
		}
	}

	return kept, nil
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
	// Offset is the 0-based rank of the anchored result in the candidate list.
	// It lets the next call size its candidate window from offset+pageSize
	// without keeping any server-side state — a hard requirement for a
	// multi-instance deployment. Resolving the page still anchors on Source:
	// the offset only sizes the fetch, it never selects the results.
	Offset int `json:"o,omitempty"`
	// Filter fingerprints the metadata filter the cursor was issued for. A
	// cursor is a position inside one filtered ordering; resuming it under a
	// different filter would silently duplicate or skip results, so the
	// mismatch is reported instead (ErrCursorFilterMismatch).
	Filter string `json:"f,omitempty"`
}

func encodeCursor(r *index.SearchResult, offset int, fingerprint string) (string, error) {
	data, err := json.Marshal(searchCursor{
		Source: r.Source.String(),
		Score:  r.Score,
		Offset: offset,
		Filter: fingerprint,
	})
	if err != nil {
		return "", errors.WithStack(err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// filterFingerprint identifies a filter by what it selects rather than by how it
// was written: reordered conditions, an int where a float was used or a date in
// another timezone all fingerprint identically. An empty filter has no
// fingerprint, so cursors issued for unfiltered searches stay interchangeable.
func filterFingerprint(filter index.Filter) string {
	if len(filter) == 0 {
		return ""
	}

	sum := sha256.Sum256(filter.CanonicalBytes())

	return hex.EncodeToString(sum[:8])
}

// resumeCursor reports how many results the given cursor skips, rejecting a
// cursor issued for another filter. An empty cursor starts at the beginning.
//
// A cursor predating this check carries no fingerprint, so it resumes an
// unfiltered search as before, and is rejected against a filtered one — the
// safe direction, since its ordering cannot be reconstructed.
func resumeCursor(cursor string, fingerprint string) (int, error) {
	if cursor == "" {
		return 0, nil
	}

	anchor, err := decodeCursor(cursor)
	if err != nil {
		return 0, errors.WithStack(err)
	}

	if anchor.Filter != fingerprint {
		return 0, errors.WithStack(ErrCursorFilterMismatch)
	}

	if anchor.Offset < 0 {
		return 0, errors.Errorf("invalid search cursor: negative offset %d", anchor.Offset)
	}

	return anchor.Offset + 1, nil
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
func paginate(results []*index.SearchResult, cursor string, pageSize int, fingerprint string) ([]*index.SearchResult, string, error) {
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
		nextCursor, err = encodeCursor(page[len(page)-1], end-1, fingerprint)
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
