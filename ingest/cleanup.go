package ingest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

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

type CleanupHandler struct {
	index index.Index
	store Store
}

func NewCleanupHandler(idx index.Index, store Store) *CleanupHandler {
	return &CleanupHandler{
		index: idx,
		store: store,
	}
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

	return nil
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
