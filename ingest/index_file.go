package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/lang"
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
	// Directory the relative image paths of the document resolve against. It
	// overrides the one derived from the source: the two part ways as soon as
	// the source is a logical identifier rather than a filesystem path (see
	// ImageBaseDir).
	imageBaseDir string
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
	ImageBaseDir string               `json:"imageBaseDir,omitempty"`
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
		ImageBaseDir: i.imageBaseDir,
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
	i.imageBaseDir = payload.ImageBaseDir
	i.metadata = payload.Metadata

	source, err := url.Parse(payload.Source)
	if err != nil {
		return errors.WithStack(err)
	}

	i.source = source

	return nil
}

// IndexFileTaskOption configures an optional aspect of an IndexFileTask, kept
// out of the constructor's positional arguments.
type IndexFileTaskOption func(*IndexFileTask)

// WithIndexFileTaskImageBaseDir sets the directory the relative image paths of
// the document resolve against, overriding the one derived from the source.
func WithIndexFileTaskImageBaseDir(dir string) IndexFileTaskOption {
	return func(t *IndexFileTask) {
		t.imageBaseDir = dir
	}
}

func NewIndexFileTask(path string, originalName string, etag string, source *url.URL, collections []model.CollectionID, metadata map[string]any, funcs ...IndexFileTaskOption) *IndexFileTask {
	t := &IndexFileTask{
		id:           task.NewID(),
		path:         path,
		originalName: originalName,
		etag:         etag,
		source:       source,
		collections:  collections,
		metadata:     metadata,
	}

	for _, fn := range funcs {
		fn(t)
	}

	return t
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

// ImageEnricher inserts, in the markdown source of a document, the textual
// description of the images it embeds — making them searchable like any other
// text. It is satisfied by markdown/imagetext.Enricher.
//
// baseDir is the directory relative image paths resolve against (empty
// disables their resolution) and progress, when non-nil, is called as
// descriptions complete. Implementations must be tolerant: an image that
// cannot be described is left alone rather than failing the document.
type ImageEnricher interface {
	Enrich(ctx context.Context, data []byte, baseDir string, progress func(done, total int)) ([]byte, error)
}

type IndexFileHandler struct {
	store             Store
	fileConverter     convert.Converter
	sourceCode        *sourcecode.Registry
	imageEnricher     ImageEnricher
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

// WithIndexFileHandlerImageEnrichment describes the images embedded in the
// markdown source of a document before it is parsed — so it applies uniformly
// to native .md files and to the output of the converters (pandoc,
// LibreOffice, GenAI OCR). A nil enricher disables it.
func WithIndexFileHandlerImageEnrichment(enricher ImageEnricher) IndexFileHandlerOptionFunc {
	return func(h *IndexFileHandler) {
		h.imageEnricher = enricher
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

// enrichImages describes the images embedded in the markdown source. It is
// best-effort: only a context error (the 2h task timeout, a cancellation) can
// fail the document — anything else leaves the source as it is.
func (h *IndexFileHandler) enrichImages(ctx context.Context, indexFileTask *IndexFileTask, data []byte, events chan task.Event) ([]byte, error) {
	progress := func(done, total int) {
		if total <= 0 {
			return
		}

		events <- task.NewEvent(
			task.WithMessage(fmt.Sprintf("describing images (%d/%d)", done, total)),
			// Image description sits between the conversion (0.05) and the
			// parsing (0.1) of the document.
			task.WithProgress(0.05+0.05*float32(done)/float32(total)),
		)
	}

	enriched, err := h.imageEnricher.Enrich(ctx, data, indexFileTask.ImageBaseDir(), progress)
	if err != nil {
		if ctx.Err() != nil {
			return nil, errors.WithStack(err)
		}

		slog.WarnContext(ctx, "could not describe document images, indexing it as is",
			slog.Any("error", errors.WithStack(err)),
		)

		return data, nil
	}

	return enriched, nil
}

// ImageBaseDir returns the directory relative image paths resolve against: the
// directory of the *original* file, not of the staged copy the handler works
// on.
//
// The explicit value set by the scheduler wins, and is the only reliable one:
// falling back on the source assumes it carries a real filesystem path, which
// is not a given. An indexer may well store a logical identifier instead — the
// CLI's --base-dir makes sources relative to a base directory, keeping a
// leading slash so they stay well-formed file URLs. Such a source still looks
// absolute, so the fallback cannot tell it apart; it yields a directory that
// does not exist, every image is silently skipped (enrichment is best-effort),
// and the document is indexed without any of its illustrations. Schedulers
// that know the real location must therefore say so.
func (i *IndexFileTask) ImageBaseDir() string {
	if i.imageBaseDir != "" {
		return i.imageBaseDir
	}

	// Only file: sources yield one — for any other scheme there is no local
	// directory to resolve against.
	if i.source == nil || i.source.Scheme != "file" || i.source.Path == "" {
		return ""
	}

	return filepath.Dir(i.source.Path)
}

// MetadataKeyLang is the metadata key carrying the dominant *natural* language
// of a document, as an ISO 639-1 code ("fr", "en", ...). It is distinct from
// the "language" key the source-code parser injects, which names a programming
// language.
const MetadataKeyLang = "lang"

// langDetectionSampleSize bounds how much of a document is fed to the language
// detector. The dominant language of a text is settled well before that, and
// the cap keeps the cost of a large document constant.
const langDetectionSampleSize = 8 << 10

// setDetectedLang records the dominant natural language of the document under
// MetadataKeyLang. Detection is best-effort and the key is left out entirely
// when it is unreliable — an absent key is honest, a wrong one silently
// excludes the document from any lang filter.
func setDetectedLang(doc parsedDocument, data []byte) {
	code, reliable := lang.Detect(langDetectionSample(data))
	if !reliable {
		return
	}

	metadata := model.Metadata(doc)
	if metadata == nil {
		metadata = map[string]any{}
	}

	metadata[MetadataKeyLang] = code

	doc.SetMetadata(metadata)
}

// langDetectionSample returns the leading langDetectionSampleSize bytes of
// data, cut back to a rune boundary so the detector is never handed a mangled
// character.
func langDetectionSample(data []byte) string {
	if len(data) <= langDetectionSampleSize {
		return string(data)
	}

	sample := data[:langDetectionSampleSize]
	for len(sample) > 0 && !utf8.Valid(sample[len(sample)-1:]) {
		sample = sample[:len(sample)-1]
	}

	return string(sample)
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
					// The handle is the reader of the next step, which closes it.
					reader = file
					events <- task.NewEvent(task.WithProgress(0.05))
					return nil
				}

				// Past this point the file is only the converter's input: the
				// reader handed to the next step is the converter's own. Closing
				// here — on the error paths as well as on success — is what keeps
				// a large sync from leaking one descriptor per converted file.
				defer file.Close()

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

				ext := filepath.Ext(indexFileTask.originalName)

				// Images are described on the markdown source, before parsing:
				// source files are excluded, they never go through markdown.
				if h.imageEnricher != nil && !h.isSourceCode(ext) {
					data, err = h.enrichImages(ctx, indexFileTask, data, events)
					if err != nil {
						return errors.WithStack(err)
					}
				}

				events <- task.NewEvent(task.WithMessage("parsing document"))

				var doc parsedDocument

				if h.isSourceCode(ext) {
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

				setDetectedLang(doc, data)

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
