package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
	"github.com/rs/xid"
)

type ManagerOptions struct {
	MaxWordPerSection int
	FileConverter     convert.Converter
}

type ManagerOptionFunc func(opts *ManagerOptions)

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
	index             index.Index
	taskRunner        task.Runner

	// Per-manager staging directory for files awaiting indexing (created
	// lazily by IndexFile, removed by CleanupTempDir).
	tempDirOnce sync.Once
	tempDir     string
	tempDirErr  error
}

type SearchOptions struct {
	MaxResults int
	// Names of the collection the query will be restricted to
	Collections []model.CollectionID
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

func (m *Manager) Search(ctx context.Context, query string, funcs ...SearchOptionFunc) ([]*index.SearchResult, error) {
	opts := NewSearchOptions(funcs...)

	collections := make([]model.CollectionID, 0)
	for _, c := range opts.Collections {
		coll, err := m.Store.GetCollectionByID(ctx, c, false)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		collections = append(collections, coll.ID())
	}

	searchResults, err := m.index.Search(ctx, query, index.SearchOptions{
		MaxResults:  opts.MaxResults,
		Collections: collections,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return searchResults, nil
}

func (m *Manager) SupportedExtensions() []string {
	if m.fileConverter == nil {
		return []string{".md"}
	}
	return m.fileConverter.SupportedExtensions()
}

type IndexFileOptions struct {
	ETag   string
	Source *url.URL
	// Names of the collection to associate with the document
	Collections []model.CollectionID
}

type IndexFileOptionFunc func(opts *IndexFileOptions)

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

	indexFileTask := NewIndexFileTask(path, filename, opts.ETag, opts.Source, opts.Collections)

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
func (m *Manager) RegisterHandlers(runner task.Runner) {
	runner.RegisterTask(TaskTypeIndexFile, NewIndexFileHandler(m.Store, m.fileConverter, m.index, m.maxWordPerSection))
	runner.RegisterTask(TaskTypeCleanup, NewCleanupHandler(m.index, m.Store))
	runner.RegisterTask(TaskTypeReindex, NewReindexHandler(m.Store, m.index, m.maxWordPerSection))
}

func NewManager(store Store, idx index.Index, taskRunner task.Runner, funcs ...ManagerOptionFunc) *Manager {
	opts := NewManagerOptions(funcs...)

	// Best-effort: sweep staging directories left behind by previous runs that
	// crashed before their indexing tasks removed the staged files. Only dirs
	// older than staleTempDirAge are removed, so a concurrently-running
	// instance's fresh directory is never touched.
	cleanStaleTempDirs(staleTempDirAge)

	manager := &Manager{
		maxWordPerSection: opts.MaxWordPerSection,
		Store:             store,
		taskRunner:        taskRunner,
		index:             idx,
		fileConverter:     opts.FileConverter,
	}

	return manager
}

// tempDirPrefix names this library's staging directories under os.TempDir().
const tempDirPrefix = "amoxtli-"

// staleTempDirAge is how old a staging directory must be before the startup
// sweep considers it orphaned.
const staleTempDirAge = 24 * time.Hour

// stagingDir returns the per-manager staging directory, creating it on first
// use.
func (m *Manager) stagingDir() (string, error) {
	m.tempDirOnce.Do(func() {
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
// shutdown, once in-flight indexing tasks have drained.
func (m *Manager) CleanupTempDir() error {
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
