package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

const TaskTypeCleanup task.Type = "cleanup"

type cleanupTaskPayload struct {
	Collections []model.CollectionID `json:"collections"`
}

type CleanupTask struct {
	id          task.ID
	collections []model.CollectionID
}

// MarshalJSON implements [task.Task].
func (t *CleanupTask) MarshalJSON() ([]byte, error) {
	payload := cleanupTaskPayload{
		Collections: t.collections,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return data, nil
}

// UnmarshalJSON implements [task.Task].
func (t *CleanupTask) UnmarshalJSON(data []byte) error {
	var payload cleanupTaskPayload

	if err := json.Unmarshal(data, &payload); err != nil {
		return errors.WithStack(err)
	}

	t.collections = payload.Collections

	return nil
}

// ID implements [task.Task].
func (t *CleanupTask) ID() task.ID {
	return t.id
}

// Type implements [task.Task].
func (t *CleanupTask) Type() task.Type {
	return TaskTypeCleanup
}

func NewCleanupTask(collections []model.CollectionID) *CleanupTask {
	return &CleanupTask{
		id:          task.NewID(),
		collections: collections,
	}
}

var _ task.Task = &CleanupTask{}

// CleanupTaskFactory rebuilds a CleanupTask from its persisted payload, used by
// persistent task runners to resume or fetch the task.
func CleanupTaskFactory(id task.ID, payload []byte) (task.Task, error) {
	t := &CleanupTask{id: id}
	if err := t.UnmarshalJSON(payload); err != nil {
		return nil, errors.WithStack(err)
	}
	return t, nil
}

type CleanupHandler struct {
	index index.Index
	store Store
	blobs blob.Store
}

// CleanupHandlerOptionFunc configures a CleanupHandler.
type CleanupHandlerOptionFunc func(h *CleanupHandler)

// WithCleanupBlobStore enables the blob garbage collection: blobs no stored
// document references any more are deleted. A nil store disables it.
func WithCleanupBlobStore(store blob.Store) CleanupHandlerOptionFunc {
	return func(h *CleanupHandler) {
		h.blobs = store
	}
}

func NewCleanupHandler(idx index.Index, store Store, funcs ...CleanupHandlerOptionFunc) *CleanupHandler {
	handler := &CleanupHandler{
		index: idx,
		store: store,
	}

	for _, fn := range funcs {
		fn(handler)
	}

	return handler
}

// Handle implements [task.Handler].
func (h *CleanupHandler) Handle(ctx context.Context, tsk task.Task, events chan task.Event) error {
	cleanupTask, ok := tsk.(*CleanupTask)
	if !ok {
		return errors.Errorf("unexpected task type '%T'", tsk)
	}

	if err := h.cleanupOrphanedDocuments(ctx, cleanupTask); err != nil {
		return errors.Wrap(err, "could not cleanup orphaned sections")
	}

	if err := h.cleanupObsoleteSections(ctx, cleanupTask); err != nil {
		return errors.Wrap(err, "could not cleanup obsolete sections")
	}

	if err := h.cleanupOrphanedBlobs(ctx); err != nil {
		return errors.Wrap(err, "could not cleanup orphaned blobs")
	}

	return nil
}

// cleanupOrphanedBlobs deletes the blobs no stored document references any
// more. Blobs are written during conversion — before the document is saved —
// so a failure downstream can leave some behind; and a document deleted (or
// re-indexed under a different image) orphans its own. There is no reference
// counting on purpose: a hash may be shared by any number of documents, and a
// periodic sweep is both simpler and safe, since a blob is only ever deleted
// once nothing points at it.
func (h *CleanupHandler) cleanupOrphanedBlobs(ctx context.Context) error {
	if h.blobs == nil {
		return nil
	}

	slog.DebugContext(ctx, "checking orphaned blobs")

	referenced, err := h.referencedBlobs(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	const deleteBatchSize = 100

	toDelete := make([]blob.Hash, 0, deleteBatchSize)
	deleted := 0

	flush := func() error {
		if len(toDelete) == 0 {
			return nil
		}

		slog.InfoContext(ctx, "deleting orphaned blobs", slog.Int("count", len(toDelete)))

		if err := h.blobs.Delete(ctx, toDelete...); err != nil {
			return errors.WithStack(err)
		}

		deleted += len(toDelete)
		toDelete = toDelete[:0]

		return nil
	}

	err = h.blobs.List(ctx, func(info blob.Info) error {
		if _, exists := referenced[info.Hash]; exists {
			return nil
		}

		toDelete = append(toDelete, info.Hash)

		if len(toDelete) >= deleteBatchSize {
			return flush()
		}

		return nil
	})
	if err != nil {
		return errors.WithStack(err)
	}

	if err := flush(); err != nil {
		return errors.WithStack(err)
	}

	slog.DebugContext(ctx, "orphaned blobs deleted", slog.Int("total", deleted))

	return nil
}

// BlobReferenceLister is the optional capability of a Store able to enumerate
// the blobs its documents reference without reading their content — typically
// from an index maintained at write time (see ingest/gorm.DocumentBlob). A
// store that does not implement it still works: the cleanup falls back to
// scanning the documents.
//
// The enumeration must be *complete*: a reference missed here is a live blob
// deleted. An implementation is expected to be maintained in the same
// transaction as the document write, and to be covered by a differential test
// against blob.ScanHashes.
type BlobReferenceLister interface {
	ListReferencedBlobs(ctx context.Context, fn func(blob.Hash) error) error
}

// referencedBlobs collects every blob hash referenced by a stored document.
// It prefers the store's own index when it exposes one, and falls back to
// reading the documents otherwise.
func (h *CleanupHandler) referencedBlobs(ctx context.Context) (map[blob.Hash]struct{}, error) {
	if lister, ok := h.store.(BlobReferenceLister); ok {
		return h.listReferencedBlobs(ctx, lister)
	}

	return h.scanReferencedBlobs(ctx)
}

// listReferencedBlobs reads the live set from the store index: one indexed
// query instead of a full pass over the corpus.
func (h *CleanupHandler) listReferencedBlobs(ctx context.Context, lister BlobReferenceLister) (map[blob.Hash]struct{}, error) {
	referenced := map[blob.Hash]struct{}{}

	err := lister.ListReferencedBlobs(ctx, func(hash blob.Hash) error {
		referenced[hash] = struct{}{}

		return nil
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return referenced, nil
}

// scanReferencedBlobs derives the live set from the content of the documents
// themselves. It is the fallback for stores without the index capability, and
// the reference behaviour the index is tested against.
func (h *CleanupHandler) scanReferencedBlobs(ctx context.Context) (map[blob.Hash]struct{}, error) {
	referenced := map[blob.Hash]struct{}{}

	page := 0
	limit := 50

	for {
		if err := ctx.Err(); err != nil {
			return nil, errors.WithStack(err)
		}

		documents, _, err := h.store.QueryDocuments(ctx, QueryDocumentsOptions{
			Page:       &page,
			Limit:      &limit,
			HeaderOnly: true,
		})
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if len(documents) == 0 {
			return referenced, nil
		}

		for _, header := range documents {
			document, err := h.store.GetDocumentByID(ctx, header.ID())
			if err != nil {
				return nil, errors.WithStack(err)
			}

			content, err := document.Content()
			if err != nil {
				return nil, errors.WithStack(err)
			}

			for _, hash := range blob.ScanHashes(content) {
				referenced[hash] = struct{}{}
			}
		}

		page++
	}
}

func (h *CleanupHandler) cleanupOrphanedDocuments(ctx context.Context, tsk *CleanupTask) error {
	slog.DebugContext(ctx, "checking orphaned document")

	count := 0
	batchSize := 5
	toDelete := make([]model.DocumentID, 0, batchSize)

	deleteCurrentBatch := func() {
		slog.InfoContext(ctx, "deleting orphaned documents", "document_ids", toDelete)

		if err := h.store.DeleteDocumentByID(ctx, toDelete...); err != nil {
			slog.ErrorContext(ctx, "could not delete obsolete sections", slog.Any("error", errors.WithStack(err)))
		}

		slog.InfoContext(ctx, "orphaned documents deleted")

		count += len(toDelete)

		toDelete = make([]model.DocumentID, 0, batchSize)

		// Prevent overwhelming the database
		time.Sleep(250 * time.Millisecond)
	}

	limit := batchSize
	orphaned := true

	for {
		documents, _, err := h.store.QueryDocuments(ctx, QueryDocumentsOptions{
			Limit:      &limit,
			HeaderOnly: true,
			Orphaned:   &orphaned,
		})
		if err != nil {
			return errors.Wrap(err, "could not query documents")
		}

		if len(documents) == 0 {
			break
		}

		documentIDs := make([]model.DocumentID, len(documents))
		for i, d := range documents {
			documentIDs[i] = d.ID()
		}

		toDelete = append(toDelete, documentIDs...)

		if len(toDelete) >= batchSize {
			deleteCurrentBatch()
		}
	}

	if len(toDelete) > 0 {
		deleteCurrentBatch()
	}

	slog.DebugContext(ctx, "orphaned documents deleted", slog.Int64("total", int64(count)))

	return nil
}

func (h *CleanupHandler) cleanupObsoleteSections(ctx context.Context, tsk *CleanupTask) error {
	slog.DebugContext(ctx, "checking obsolete sections")

	const checkBatchSize = 500
	const deleteBatchSize = 5000

	count := 0
	checkBatch := make([]model.SectionID, 0, checkBatchSize)
	toDelete := make([]model.SectionID, 0, deleteBatchSize)

	flushDeleteBatch := func() {
		if len(toDelete) == 0 {
			return
		}
		slog.InfoContext(ctx, "deleting obsolete sections from index", slog.Int("count", len(toDelete)))
		if err := h.index.DeleteByID(ctx, toDelete...); err != nil {
			slog.ErrorContext(ctx, "could not delete obsolete sections", slog.Any("error", errors.WithStack(err)))
		}
		toDelete = toDelete[:0]
	}

	flushCheckBatch := func() {
		if len(checkBatch) == 0 {
			return
		}
		existMap, err := h.store.SectionsExist(ctx, checkBatch)
		if err != nil {
			slog.ErrorContext(ctx, "could not bulk-check sections existence", slog.Any("error", errors.WithStack(err)))
			checkBatch = checkBatch[:0]
			return
		}
		for _, id := range checkBatch {
			if !existMap[id] {
				toDelete = append(toDelete, id)
			}
		}
		checkBatch = checkBatch[:0]
		if len(toDelete) >= deleteBatchSize {
			flushDeleteBatch()
		}
	}

	err := h.index.All(ctx, func(id model.SectionID) bool {
		count++
		checkBatch = append(checkBatch, id)
		if len(checkBatch) >= checkBatchSize {
			flushCheckBatch()
		}
		return true
	})
	if err != nil {
		return errors.WithStack(err)
	}

	flushCheckBatch()
	flushDeleteBatch()

	slog.DebugContext(ctx, "all sections checked", slog.Int64("total", int64(count)))

	return nil
}

var _ task.Handler = &CleanupHandler{}
