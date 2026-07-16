package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/workflow"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

const TaskTypeIndexFile task.Type = "index_file"

type IndexFileTask struct {
	id           task.ID
	path         string
	originalName string
	etag         string
	source       *url.URL
	// Names of the collection to associate with the document
	collections []model.CollectionID
	// Arbitrary document metadata used for filtering at search time.
	metadata map[string]any
}

type indexTaskPayload struct {
	Path         string               `json:"path"`
	OriginalName string               `json:"originalName"`
	Etag         string               `json:"etag"`
	Source       string               `json:"source"`
	Collections  []model.CollectionID `json:"collections"`
	Metadata     map[string]any       `json:"metadata,omitempty"`
}

// MarshalJSON implements [task.Task].
func (i *IndexFileTask) MarshalJSON() ([]byte, error) {
	var sourceStr string
	if i.source != nil {
		sourceStr = i.source.String()
	}

	payload := indexTaskPayload{
		Path:         i.path,
		OriginalName: i.originalName,
		Etag:         i.etag,
		Source:       sourceStr,
		Collections:  i.collections,
		Metadata:     i.metadata,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return data, nil
}

// UnmarshalJSON implements [task.Task].
func (i *IndexFileTask) UnmarshalJSON(data []byte) error {
	var payload indexTaskPayload

	if err := json.Unmarshal(data, &payload); err != nil {
		return errors.WithStack(err)
	}

	i.collections = payload.Collections
	i.etag = payload.Etag
	i.originalName = payload.OriginalName
	i.path = payload.Path
	i.metadata = payload.Metadata

	source, err := url.Parse(payload.Source)
	if err != nil {
		return errors.WithStack(err)
	}

	i.source = source

	return nil
}

func NewIndexFileTask(path string, originalName string, etag string, source *url.URL, collections []model.CollectionID, metadata map[string]any) *IndexFileTask {
	return &IndexFileTask{
		id:           task.NewID(),
		path:         path,
		originalName: originalName,
		etag:         etag,
		source:       source,
		collections:  collections,
		metadata:     metadata,
	}
}

// ID implements [task.Task].
func (i *IndexFileTask) ID() task.ID {
	return i.id
}

// Type implements [task.Task].
func (i *IndexFileTask) Type() task.Type {
	return TaskTypeIndexFile
}

var _ task.Task = &IndexFileTask{}

// IndexFileTaskFactory rebuilds an IndexFileTask from its persisted payload,
// used by persistent task runners to resume or fetch the task.
func IndexFileTaskFactory(id task.ID, payload []byte) (task.Task, error) {
	t := &IndexFileTask{id: id}
	if err := t.UnmarshalJSON(payload); err != nil {
		return nil, errors.WithStack(err)
	}
	return t, nil
}

const indexFileTaskTimeout = 2 * time.Hour

type IndexFileHandler struct {
	store             Store
	fileConverter     convert.Converter
	sourceCode        *sourcecode.Registry
	index             index.Index
	maxWordPerSection int
}

type IndexFileHandlerOptionFunc func(h *IndexFileHandler)

// WithIndexFileHandlerSourceCode enables source-code parsing for the file
// extensions registered in the registry. A nil registry disables it.
func WithIndexFileHandlerSourceCode(registry *sourcecode.Registry) IndexFileHandlerOptionFunc {
	return func(h *IndexFileHandler) {
		h.sourceCode = registry
	}
}

func NewIndexFileHandler(store Store, fileConverter convert.Converter, idx index.Index, maxWordPerSection int, funcs ...IndexFileHandlerOptionFunc) *IndexFileHandler {
	handler := &IndexFileHandler{
		store:             store,
		fileConverter:     fileConverter,
		index:             idx,
		maxWordPerSection: maxWordPerSection,
	}

	for _, fn := range funcs {
		fn(handler)
	}

	return handler
}

// parsedDocument is the mutable document produced by the parsing step, common
// to markdown and source-code documents.
type parsedDocument interface {
	model.Document
	SetSource(source *url.URL)
	SetETag(etag string)
	SetMetadata(metadata map[string]any)
	AddCollection(coll model.Collection)
}

// isSourceCode returns true when the file must be parsed as source code.
func (h *IndexFileHandler) isSourceCode(ext string) bool {
	if h.sourceCode == nil {
		return false
	}

	_, exists := h.sourceCode.Lookup(ext)

	return exists
}

// Handle implements [task.Handler].
func (h *IndexFileHandler) Handle(ctx context.Context, tsk task.Task, events chan task.Event) error {
	// Add a 2-hour timeout for the entire task execution
	ctx, cancel := context.WithTimeout(ctx, indexFileTaskTimeout)
	defer cancel()

	indexFileTask, ok := tsk.(*IndexFileTask)
	if !ok {
		return errors.Errorf("unexpected task type '%T'", tsk)
	}

	defer func() {
		if err := os.Remove(indexFileTask.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.ErrorContext(ctx, "could not remove file", slog.Any("error", errors.WithStack(err)))
		}
	}()

	var document model.Document

	var reader io.ReadCloser

	wf := workflow.New(
		workflow.StepFunc(
			func(ctx context.Context) error {
				file, err := os.Open(indexFileTask.path)
				if err != nil {
					return errors.WithStack(err)
				}

				ext := filepath.Ext(indexFileTask.originalName)
				if ext == ".md" || h.isSourceCode(ext) || h.fileConverter == nil {
					reader = file
					events <- task.NewEvent(task.WithProgress(0.05))
					return nil
				}

				supportedExtensions := h.fileConverter.SupportedExtensions()

				if supported := slices.Contains(supportedExtensions, ext); !supported {
					return errors.Wrapf(convert.ErrNotSupported, "file extension '%s' is not supported by the file converter", ext)
				}

				events <- task.NewEvent(task.WithMessage("converting document"), task.WithProgress(0.01))

				readCloser, err := h.fileConverter.Convert(ctx, indexFileTask.originalName, file)
				if err != nil {
					return errors.WithStack(err)
				}

				reader = readCloser

				events <- task.NewEvent(task.WithProgress(0.05))

				return nil
			},
			nil,
		),
		workflow.StepFunc(
			func(ctx context.Context) error {
				defer reader.Close()

				data, err := io.ReadAll(reader)
				if err != nil {
					return errors.WithStack(err)
				}

				events <- task.NewEvent(task.WithMessage("parsing document"))

				var doc parsedDocument

				if ext := filepath.Ext(indexFileTask.originalName); h.isSourceCode(ext) {
					doc, err = sourcecode.Parse(
						indexFileTask.originalName,
						data,
						sourcecode.WithMaxWordPerSection(h.maxWordPerSection),
						sourcecode.WithRegistry(h.sourceCode),
					)
				} else {
					doc, err = markdown.Parse(
						data,
						markdown.WithMaxWordPerSection(h.maxWordPerSection),
					)
				}
				if err != nil {
					return errors.Wrap(err, "could not parse document")
				}

				events <- task.NewEvent(task.WithMessage("document parsed"))

				if indexFileTask.source != nil {
					doc.SetSource(indexFileTask.source)
				}

				if doc.Source() == nil {
					return errors.Errorf("document source missing (document header: %s)", data[0:min(len(data), 512)])
				}

				if indexFileTask.etag != "" {
					doc.SetETag(indexFileTask.etag)
				}

				// Merge user-supplied metadata over the parser-injected base
				// (e.g. type/language for source code); user values win.
				if len(indexFileTask.metadata) > 0 {
					metadata := model.Metadata(doc)
					if metadata == nil {
						metadata = map[string]any{}
					}

					maps.Copy(metadata, indexFileTask.metadata)

					doc.SetMetadata(metadata)
				}

				if len(indexFileTask.collections) == 0 {
					return errors.New("no specified target collections")
				}

				for _, collectionID := range indexFileTask.collections {
					coll, err := h.store.GetCollectionByID(ctx, collectionID, false)
					if err != nil {
						return errors.WithStack(err)
					}

					doc.AddCollection(coll)
				}

				document = doc

				events <- task.NewEvent(task.WithProgress(0.1))

				return nil
			},
			nil,
		),
		workflow.StepFunc(
			func(ctx context.Context) error {
				events <- task.NewEvent(task.WithMessage("saving document"))

				if err := h.store.SaveDocuments(ctx, document); err != nil {
					return errors.WithStack(err)
				}

				events <- task.NewEvent(task.WithProgress(0.2), task.WithMessage("document saved"))

				return nil
			},
			func(ctx context.Context) error {
				if err := h.store.DeleteDocumentBySource(ctx, document.Source()); err != nil {
					return errors.WithStack(err)
				}

				return nil
			},
		),
		workflow.StepFunc(
			func(ctx context.Context) error {
				onProgress := func(p float32) {
					events <- task.NewEvent(task.WithProgress(0.2 + (0.7 * p)))
				}

				events <- task.NewEvent(task.WithMessage("indexing document"))

				if err := h.index.Index(ctx, document, index.WithOnProgress(onProgress)); err != nil {
					return errors.WithStack(err)
				}

				events <- task.NewEvent(task.WithMessage("document indexed"))

				return nil
			},
			func(ctx context.Context) error {
				if err := h.index.DeleteBySource(ctx, document.Source()); err != nil {
					return errors.WithStack(err)
				}

				return nil
			},
		),
	)
	if err := wf.Execute(ctx); err != nil {
		return errors.WithStack(err)
	}

	events <- task.NewEvent(task.WithProgress(1), task.WithMessage("done"))

	return nil
}

var _ task.Handler = &IndexFileHandler{}
