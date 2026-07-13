package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

const TaskTypeReindex task.Type = "reindex"

type reindexTaskPayload struct {
	CollectionID model.CollectionID `json:"collectionID"`
}

// ReindexTask rebuilds the index from the stored documents.
// An empty CollectionID reindexes the whole store.
type ReindexTask struct {
	id           task.ID
	collectionID model.CollectionID
}

// MarshalJSON implements [task.Task].
func (t *ReindexTask) MarshalJSON() ([]byte, error) {
	payload := reindexTaskPayload{
		CollectionID: t.collectionID,
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return data, nil
}

// UnmarshalJSON implements [task.Task].
func (t *ReindexTask) UnmarshalJSON(data []byte) error {
	var payload reindexTaskPayload

	if err := json.Unmarshal(data, &payload); err != nil {
		return errors.WithStack(err)
	}

	t.collectionID = payload.CollectionID

	return nil
}

// ID implements [task.Task].
func (t *ReindexTask) ID() task.ID {
	return t.id
}

// Type implements [task.Task].
func (t *ReindexTask) Type() task.Type {
	return TaskTypeReindex
}

// CollectionID returns the collection ID to reindex (empty means all).
func (t *ReindexTask) CollectionID() model.CollectionID {
	return t.collectionID
}

// NewReindexTask creates a task reindexing a single collection,
// or the whole store if collectionID is empty.
func NewReindexTask(collectionID model.CollectionID) *ReindexTask {
	return &ReindexTask{
		id:           task.NewID(),
		collectionID: collectionID,
	}
}

var _ task.Task = &ReindexTask{}

// ReindexTaskFactory rebuilds a ReindexTask from its persisted payload, used by
// persistent task runners to resume or fetch the task.
func ReindexTaskFactory(id task.ID, payload []byte) (task.Task, error) {
	t := &ReindexTask{id: id}
	if err := t.UnmarshalJSON(payload); err != nil {
		return nil, errors.WithStack(err)
	}
	return t, nil
}

type ReindexHandler struct {
	store             Store
	index             index.Index
	maxWordPerSection int
}

func NewReindexHandler(store Store, idx index.Index, maxWordPerSection int) *ReindexHandler {
	return &ReindexHandler{
		store:             store,
		index:             idx,
		maxWordPerSection: maxWordPerSection,
	}
}

// Handle implements [task.Handler].
func (h *ReindexHandler) Handle(ctx context.Context, tsk task.Task, events chan task.Event) error {
	slog.DebugContext(ctx, "reindex handler started")

	select {
	case <-ctx.Done():
		slog.DebugContext(ctx, "reindex handler context cancelled at start")
		return errors.WithStack(ctx.Err())
	default:
	}

	reindexTask, ok := tsk.(*ReindexTask)
	if !ok {
		return errors.Errorf("unexpected task type '%T'", tsk)
	}

	collectionID := reindexTask.collectionID

	slog.DebugContext(ctx, "reindex handler sending first event")
	events <- task.NewEvent(task.WithMessage("retrieving total documents"))
	slog.DebugContext(ctx, "reindex handler first event sent")

	// Get all documents (optionally filtered by collection)
	limit := 1
	page := 1
	var totalDocuments int64 = 0

	var err error
	var total int64

	// First, get the total count
	if collectionID != "" {
		_, total, err = h.store.QueryDocumentsByCollectionID(ctx, collectionID, QueryDocumentsOptions{
			Limit:      &limit,
			Page:       &page,
			HeaderOnly: true,
		})
	} else {
		_, total, err = h.store.QueryDocuments(ctx, QueryDocumentsOptions{
			Limit:      &limit,
			Page:       &page,
			HeaderOnly: true,
		})
	}

	if err != nil {
		return errors.Wrap(err, "could not query documents")
	}
	totalDocuments = total

	if totalDocuments == 0 {
		events <- task.NewEvent(task.WithMessage("no document to reindex"), task.WithProgress(1))
		return nil
	}

	// First, delete all existing index entries
	events <- task.NewEvent(task.WithMessage("deleting existing index entries"))

	deletedCount := 0

	allDocumentsProcessed := false
	docPage := 1
	docLimit := 50

	for !allDocumentsProcessed {
		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		default:
		}

		var documents []model.PersistedDocument
		var count int64

		if collectionID != "" {
			documents, count, err = h.store.QueryDocumentsByCollectionID(ctx, collectionID, QueryDocumentsOptions{
				Limit: &docLimit,
				Page:  &docPage,
			})
		} else {
			documents, count, err = h.store.QueryDocuments(ctx, QueryDocumentsOptions{
				Limit: &docLimit,
				Page:  &docPage,
			})
		}

		if err != nil {
			return errors.Wrap(err, "could not query documents")
		}

		if len(documents) == 0 || count == 0 {
			allDocumentsProcessed = true
			break
		}

		events <- task.NewEvent(task.WithMessage(fmt.Sprintf("deleting documents (total: %d, batch: %d, batch size: %d)", count, docPage, docLimit)))

		// Delete index entries for these documents
		for _, doc := range documents {
			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			default:
			}

			source := doc.Source()
			if source != nil {
				if err := h.index.DeleteBySource(ctx, source); err != nil {
					slog.ErrorContext(ctx, "could not delete index entries for document", slog.Any("error", errors.WithStack(err)))
				}
				deletedCount++
			}
		}

		// Update progress
		progress := float32(docPage*docLimit) / float32(totalDocuments)
		if progress > 1 {
			progress = 1
		}
		events <- task.NewEvent(task.WithProgress(0.3*progress), task.WithMessage("deleting old index entries"))

		if len(documents) < docLimit {
			allDocumentsProcessed = true
		}
		docPage++
	}

	slog.InfoContext(ctx, "deleted index entries", "count", deletedCount)

	// Now re-index all documents
	events <- task.NewEvent(task.WithMessage("reindexing documents"))

	allDocumentsProcessed = false
	docPage = 1

	for !allDocumentsProcessed {
		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		default:
		}

		var documents []model.PersistedDocument
		var count int64

		if collectionID != "" {
			documents, count, err = h.store.QueryDocumentsByCollectionID(ctx, collectionID, QueryDocumentsOptions{
				Limit: &docLimit,
				Page:  &docPage,
			})
		} else {
			documents, count, err = h.store.QueryDocuments(ctx, QueryDocumentsOptions{
				Limit: &docLimit,
				Page:  &docPage,
			})
		}

		if err != nil {
			return errors.Wrap(err, "could not query documents")
		}

		if len(documents) == 0 || count == 0 {
			allDocumentsProcessed = true
			break
		}

		for i, doc := range documents {
			select {
			case <-ctx.Done():
				return errors.WithStack(ctx.Err())
			default:
			}

			// Re-index the document (documents already have their collections from the store)
			if err := h.index.Index(ctx, doc); err != nil {
				slog.ErrorContext(ctx, "could not index document", slog.Any("error", errors.WithStack(err)))
			}

			// Update progress (30% to 100%)
			docProgress := float32((docPage-1)*docLimit+i+1) / float32(totalDocuments)
			progress := 0.3 + (0.7 * docProgress)
			events <- task.NewEvent(task.WithProgress(progress), task.WithMessage("reindexing"))
		}

		if len(documents) < docLimit {
			allDocumentsProcessed = true
		}
		docPage++
	}

	events <- task.NewEvent(task.WithProgress(1), task.WithMessage("reindexing finished"))

	return nil
}

var _ task.Handler = &ReindexHandler{}
