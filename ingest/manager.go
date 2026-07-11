package ingest

import (
	"context"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sync"

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

	tempDir, err := sharedTempDir()
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

	manager := &Manager{
		maxWordPerSection: opts.MaxWordPerSection,
		Store:             store,
		taskRunner:        taskRunner,
		index:             idx,
		fileConverter:     opts.FileConverter,
	}

	return manager
}

var (
	createTempDirOnce sync.Once
	createTempDirErr  error
	tempDir           string
)

func sharedTempDir() (string, error) {
	createTempDirOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "amoxtli-*")
		if err != nil {
			createTempDirErr = errors.WithStack(err)
			return
		}

		tempDir = tmp
	})
	if createTempDirErr != nil {
		return "", errors.WithStack(createTempDirErr)
	}

	return tempDir, nil
}
