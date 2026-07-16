package gorm

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/internal/syncx"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// progressPersistInterval throttles how often mid-flight progress/message
// updates are written to the database, to avoid hammering it with the many
// events a long-running task emits. Status transitions and terminal states are
// always persisted immediately.
const progressPersistInterval = time.Second

type queuedTask struct {
	task   task.Task
	ctx    context.Context
	cancel context.CancelFunc
}

// TaskRunner is a persistent [task.Runner] backed by a gorm database. Scheduled
// tasks are serialized (through their json.Marshaler) and stored, so pending
// tasks — and tasks that were still running when the process stopped — are
// resumed on the next Run. Deserialization relies on factories registered with
// RegisterFactory (task.PersistentRunner).
type TaskRunner struct {
	getDatabase func(ctx context.Context) (*gorm.DB, error)

	handlers  syncx.Map[task.Type, task.Handler]
	factories syncx.Map[task.Type, task.Factory]

	queue     chan queuedTask
	errOnFull bool

	cancelFuncs syncx.Map[task.ID, context.CancelFunc]

	parallelism     int
	cleanupDelay    time.Duration
	cleanupInterval time.Duration
}

// RegisterTask implements [task.Runner].
func (r *TaskRunner) RegisterTask(taskType task.Type, handler task.Handler) {
	r.handlers.Store(taskType, handler)
}

// RegisterFactory implements [task.PersistentRunner]. The factory rebuilds a
// concrete task from its persisted payload so it can be resumed or fetched.
func (r *TaskRunner) RegisterFactory(taskType task.Type, factory task.Factory) {
	r.factories.Store(taskType, factory)
}

// ScheduleTask implements [task.Runner]. The task is persisted as pending
// before being enqueued, so it survives a restart even if it is never picked up
// by a worker in this process.
func (r *TaskRunner) ScheduleTask(ctx context.Context, tsk task.Task) error {
	payload, err := tsk.MarshalJSON()
	if err != nil {
		return errors.Wrap(err, "could not marshal task payload")
	}

	row := &Task{
		ID:          string(tsk.ID()),
		Type:        string(tsk.Type()),
		Payload:     payload,
		Status:      task.StatusPending,
		ScheduledAt: time.Now(),
	}

	if err := r.save(ctx, row); err != nil {
		return errors.WithStack(err)
	}

	return r.enqueue(ctx, tsk)
}

// enqueue wires a cancelable context to the task and pushes it onto the work
// queue. The context is derived from Background (not the scheduling context) so
// an in-flight task is not aborted merely because the caller returned; it is
// canceled through CancelTask or when the whole runner stops.
func (r *TaskRunner) enqueue(ctx context.Context, tsk task.Task) error {
	taskID := tsk.ID()

	taskCtx, cancelFn := context.WithCancel(context.Background())
	r.cancelFuncs.Store(taskID, cancelFn)

	qt := queuedTask{task: tsk, ctx: taskCtx, cancel: cancelFn}

	if r.errOnFull {
		select {
		case r.queue <- qt:
		default:
			cancelFn()
			r.cancelFuncs.Delete(taskID)
			if err := r.markFailed(ctx, taskID, errors.WithStack(task.ErrQueueFull)); err != nil {
				slog.ErrorContext(ctx, "could not persist queue-full task state", slog.Any("error", errors.WithStack(err)))
			}
			return errors.WithStack(task.ErrQueueFull)
		}
	} else {
		r.queue <- qt
	}

	return nil
}

// Run implements [task.Runner]. It first resumes persisted work (pending tasks
// and tasks left running by a previous, interrupted process), then serves the
// queue with a fixed worker pool until the context is canceled. On shutdown it
// lets in-flight tasks drain before returning.
func (r *TaskRunner) Run(ctx context.Context) error {
	// Recover orphans BEFORE starting the workers: any task still marked running
	// at this point was left behind by a previous, interrupted process (no worker
	// of this process has claimed anything yet), so it is safe to reset it to
	// pending. Doing this after workers start would risk clobbering a task this
	// process is actively running.
	if err := r.recoverOrphans(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "could not recover orphaned tasks", slog.Any("error", errors.WithStack(err)))
	}

	var wg sync.WaitGroup
	for i := 0; i < r.parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case qt, ok := <-r.queue:
					if !ok {
						return
					}
					r.executeTask(qt)
				}
			}
		}()
	}

	// Enqueue pending work now that workers are consuming the queue (so a large
	// backlog cannot stall on the buffer). Duplicate enqueues — a task both
	// scheduled by this process and picked up here — are made harmless by the
	// claim guard in executeTask.
	if err := r.enqueuePending(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "could not enqueue pending tasks", slog.Any("error", errors.WithStack(err)))
	}

	// Cleanup goroutine: purge tasks that finished long enough ago.
	go func() {
		ticker := time.NewTicker(r.cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.cleanupExpired(ctx); err != nil {
					slog.WarnContext(ctx, "could not clean up expired tasks", slog.Any("error", errors.WithStack(err)))
				}
			}
		}
	}()

	<-ctx.Done()
	close(r.queue)
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// recoverOrphans resets every task still marked running back to pending. It runs
// once, before any worker starts, so the only running tasks it can see are those
// left behind by a previous, interrupted process. They are re-run from the start
// (the ingestion handlers are idempotent).
func (r *TaskRunner) recoverOrphans(ctx context.Context) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	res := db.WithContext(ctx).Model(&Task{}).
		Where("status = ?", task.StatusRunning).
		Update("status", task.StatusPending)
	if res.Error != nil {
		return errors.WithStack(res.Error)
	}
	if res.RowsAffected > 0 {
		slog.InfoContext(ctx, "reset orphaned running tasks to pending", slog.Int64("count", res.RowsAffected))
	}

	return nil
}

// enqueuePending re-enqueues every task still marked pending so it is executed
// by the worker pool. Deserialization uses the registered factories; a task
// whose type has no factory cannot be rebuilt and is skipped.
func (r *TaskRunner) enqueuePending(ctx context.Context) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	var rows []Task
	if err := db.WithContext(ctx).
		Where("status = ?", task.StatusPending).
		Order("scheduled_at ASC").
		Find(&rows).Error; err != nil {
		return errors.WithStack(err)
	}

	for i := range rows {
		row := rows[i]

		factory, exists := r.factories.Load(task.Type(row.Type))
		if !exists {
			slog.WarnContext(ctx, "no factory registered for persisted task type, skipping resume",
				slog.String("taskID", row.ID), slog.String("taskType", row.Type))
			continue
		}

		tsk, err := factory(task.ID(row.ID), row.Payload)
		if err != nil {
			slog.ErrorContext(ctx, "could not rebuild persisted task, skipping resume",
				slog.String("taskID", row.ID), slog.Any("error", errors.WithStack(err)))
			continue
		}

		slog.DebugContext(ctx, "resuming persisted task",
			slog.String("taskID", row.ID), slog.String("taskType", row.Type))

		if err := r.enqueue(ctx, tsk); err != nil {
			slog.ErrorContext(ctx, "could not resume persisted task",
				slog.String("taskID", row.ID), slog.Any("error", errors.WithStack(err)))
		}
	}

	return nil
}

func (r *TaskRunner) executeTask(qt queuedTask) {
	tsk := qt.task
	taskCtx := qt.ctx
	taskID := tsk.ID()

	ctx := slogx.WithAttrs(taskCtx,
		slog.String("taskID", string(taskID)),
		slog.String("taskType", string(tsk.Type())),
	)

	defer func() {
		r.cancelFuncs.Delete(taskID)

		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok {
				err = errors.Errorf("%+v", recovered)
			}
			slog.ErrorContext(ctx, "recovered panic while running task", slog.Any("error", errors.WithStack(err)))
			if err := r.markFailed(ctx, taskID, errors.WithStack(err)); err != nil {
				slog.ErrorContext(ctx, "could not persist recovered task state", slog.Any("error", errors.WithStack(err)))
			}
		}
	}()

	// Atomically claim the task: transition pending -> running only if it is
	// still pending. A task can legitimately be enqueued twice (scheduled in
	// this process while also being resumed at startup, or resumed by several
	// runners sharing the database); the claim guard ensures exactly one of
	// those executions proceeds.
	claimed, err := r.claim(ctx, taskID)
	if err != nil {
		slog.ErrorContext(ctx, "could not claim task", slog.Any("error", errors.WithStack(err)))
		return
	}
	if !claimed {
		slog.DebugContext(ctx, "task already claimed, skipping duplicate execution")
		return
	}

	handler, exists := r.handlers.Load(tsk.Type())
	if !exists {
		if err := r.markFailed(ctx, taskID, errors.Errorf("no handler registered for task type '%s'", tsk.Type())); err != nil {
			slog.ErrorContext(ctx, "could not persist task state", slog.Any("error", errors.WithStack(err)))
		}
		return
	}

	events := make(chan task.Event, 100)

	var eventsWg sync.WaitGroup
	eventsWg.Add(1)
	go func() {
		defer eventsWg.Done()

		var (
			lastPersist time.Time
			progress    float32
			message     string
			dirty       bool
		)

		flush := func() {
			if !dirty {
				return
			}
			if err := r.updateState(ctx, taskID, map[string]any{"progress": progress, "message": message}); err != nil {
				slog.WarnContext(ctx, "could not persist task progress", slog.Any("error", errors.WithStack(err)))
			}
			lastPersist = time.Now()
			dirty = false
		}

		for e := range events {
			if e.Progress != nil {
				progress = float32(max(min(*e.Progress, 1), 0))
				dirty = true
			}
			if e.Message != nil {
				message = *e.Message
				dirty = true
			}
			// Throttle intermediate writes; the terminal state is persisted
			// separately once the handler returns.
			if dirty && time.Since(lastPersist) >= progressPersistInterval {
				flush()
			}
		}
		// Persist whatever progress/message the throttle skipped, so the last
		// pre-terminal state is durable.
		flush()
	}()

	start := time.Now()

	err = handler.Handle(taskCtx, tsk, events)

	close(events)
	eventsWg.Wait()

	if errors.Is(err, task.ErrCanceled) {
		slog.DebugContext(ctx, "task was canceled")
		if err := r.markFailed(ctx, taskID, errors.WithStack(task.ErrCanceled)); err != nil {
			slog.ErrorContext(ctx, "could not persist canceled task state", slog.Any("error", errors.WithStack(err)))
		}
		return
	}

	if err != nil {
		err = errors.WithStack(err)
		slog.ErrorContext(ctx, "task failed", slog.Any("error", err))
		if err := r.markFailed(ctx, taskID, err); err != nil {
			slog.ErrorContext(ctx, "could not persist failed task state", slog.Any("error", errors.WithStack(err)))
		}
		return
	}

	slog.DebugContext(ctx, "task finished", slog.Duration("duration", time.Since(start)))
	now := time.Now()
	if err := r.updateState(ctx, taskID, map[string]any{
		"status":      task.StatusSucceeded,
		"finished_at": &now,
		"progress":    float32(1),
	}); err != nil {
		slog.ErrorContext(ctx, "could not persist succeeded task state", slog.Any("error", errors.WithStack(err)))
	}
}

// CancelTask implements [task.Runner].
func (r *TaskRunner) CancelTask(ctx context.Context, id task.ID) error {
	state, err := r.GetTaskState(ctx, id)
	if err != nil {
		return errors.WithStack(err)
	}

	if state.Status != task.StatusPending && state.Status != task.StatusRunning {
		return errors.WithStack(task.ErrCanceled)
	}

	if cancelFn, exists := r.cancelFuncs.Load(id); exists {
		cancelFn()
		r.cancelFuncs.Delete(id)
	}

	if err := r.markFailed(ctx, id, errors.WithStack(task.ErrCanceled)); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// GetTask implements [task.Runner]. It rebuilds the concrete task from its
// persisted payload using the registered factory.
func (r *TaskRunner) GetTask(ctx context.Context, id task.ID) (task.Task, error) {
	row, err := r.getRow(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	factory, exists := r.factories.Load(task.Type(row.Type))
	if !exists {
		return nil, errors.Errorf("no factory registered for task type '%s'", row.Type)
	}

	tsk, err := factory(task.ID(row.ID), row.Payload)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return tsk, nil
}

// GetTaskState implements [task.Runner].
func (r *TaskRunner) GetTaskState(ctx context.Context, id task.ID) (*task.State, error) {
	row, err := r.getRow(ctx, id)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return rowToState(row), nil
}

// ListTasks implements [task.Runner].
func (r *TaskRunner) ListTasks(ctx context.Context) ([]task.StateHeader, error) {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var rows []Task
	if err := db.WithContext(ctx).Order("scheduled_at ASC").Find(&rows).Error; err != nil {
		return nil, errors.WithStack(err)
	}

	headers := make([]task.StateHeader, 0, len(rows))
	for i := range rows {
		headers = append(headers, rowToState(&rows[i]).StateHeader)
	}

	return headers, nil
}

// --- persistence helpers ---

func (r *TaskRunner) save(ctx context.Context, row *Task) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := db.WithContext(ctx).Save(row).Error; err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// claim atomically transitions a task from pending to running, returning true
// only if this call performed the transition. It is the guard that makes a
// task safe to enqueue more than once.
func (r *TaskRunner) claim(ctx context.Context, id task.ID) (bool, error) {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return false, errors.WithStack(err)
	}
	res := db.WithContext(ctx).Model(&Task{}).
		Where("id = ? AND status = ?", string(id), task.StatusPending).
		Updates(map[string]any{"status": task.StatusRunning})
	if res.Error != nil {
		return false, errors.WithStack(res.Error)
	}
	return res.RowsAffected == 1, nil
}

func (r *TaskRunner) updateState(ctx context.Context, id task.ID, fields map[string]any) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := db.WithContext(ctx).Model(&Task{}).Where("id = ?", string(id)).Updates(fields).Error; err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (r *TaskRunner) markFailed(ctx context.Context, id task.ID, cause error) error {
	now := time.Now()
	return r.updateState(ctx, id, map[string]any{
		"status":      task.StatusFailed,
		"finished_at": &now,
		"error":       cause.Error(),
	})
}

func (r *TaskRunner) getRow(ctx context.Context, id task.ID) (*Task, error) {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var row Task
	if err := db.WithContext(ctx).Where("id = ?", string(id)).First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.WithStack(task.ErrNotFound)
		}
		return nil, errors.WithStack(err)
	}

	return &row, nil
}

func (r *TaskRunner) cleanupExpired(ctx context.Context) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	cutoff := time.Now().Add(-r.cleanupDelay)
	if err := db.WithContext(ctx).
		Where("finished_at IS NOT NULL AND finished_at < ?", cutoff).
		Delete(&Task{}).Error; err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func rowToState(row *Task) *task.State {
	state := &task.State{
		StateHeader: task.StateHeader{
			ID:          task.ID(row.ID),
			Type:        task.Type(row.Type),
			ScheduledAt: row.ScheduledAt,
			Status:      task.Status(row.Status),
		},
		Progress: row.Progress,
		Message:  row.Message,
	}
	if row.FinishedAt != nil {
		state.FinishedAt = *row.FinishedAt
	}
	if row.Error != "" {
		state.Error = errors.New(row.Error)
	}
	return state
}

// NewTaskRunner creates a persistent runner backed by db. The schema is migrated
// lazily on the first database access. Register the task handlers with
// RegisterTask and the deserialization factories with RegisterFactory before
// calling Run so pending tasks can be resumed.
func NewTaskRunner(db *gorm.DB, parallelism int, cleanupDelay time.Duration, cleanupInterval time.Duration) *TaskRunner {
	return NewTaskRunnerWithQueue(db, parallelism, 10_000, false, cleanupDelay, cleanupInterval)
}

// NewTaskRunnerWithQueue is NewTaskRunner with control over the in-memory queue
// size and whether ScheduleTask fails (ErrQueueFull) when the queue is full.
func NewTaskRunnerWithQueue(db *gorm.DB, parallelism int, queueSize int, errOnFull bool, cleanupDelay time.Duration, cleanupInterval time.Duration) *TaskRunner {
	return &TaskRunner{
		getDatabase:     createGetDatabase(db, &Task{}),
		parallelism:     parallelism,
		queue:           make(chan queuedTask, queueSize),
		errOnFull:       errOnFull,
		handlers:        syncx.Map[task.Type, task.Handler]{},
		factories:       syncx.Map[task.Type, task.Factory]{},
		cancelFuncs:     syncx.Map[task.ID, context.CancelFunc]{},
		cleanupDelay:    cleanupDelay,
		cleanupInterval: cleanupInterval,
	}
}

var (
	_ task.Runner           = &TaskRunner{}
	_ task.PersistentRunner = &TaskRunner{}
)
