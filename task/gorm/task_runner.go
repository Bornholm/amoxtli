package gorm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/internal/syncx"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
	"github.com/rs/xid"
	"gorm.io/gorm"
)

// progressPersistInterval throttles how often mid-flight progress/message
// updates are written to the database, to avoid hammering it with the many
// events a long-running task emits. Status transitions and terminal states are
// always persisted immediately.
const progressPersistInterval = time.Second

const (
	defaultLeaseDuration = time.Minute
	defaultPollInterval  = 2 * time.Second
)

// TaskRunner is a persistent [task.Runner] backed by a gorm database. Scheduled
// tasks are serialized (through their json.Marshaler) and stored; the database
// is the only queue, so several runners — in one process or in several — can
// share it and split the work between them.
//
// A worker claims a pending task by atomically moving it to running under a
// lease it owns, and keeps extending the lease while the handler runs. A task
// whose lease expires (its runner crashed or lost the database) goes back to
// pending and is picked up again, from the start: handlers must be idempotent,
// which the ingestion handlers are. Leases rely on the clocks of the machines
// involved: a clock skew larger than the lease margin can make a task run
// twice.
//
// Deserialization relies on factories registered with RegisterFactory
// (task.PersistentRunner); a task scheduled by this runner is also kept in
// memory, so it can run here without one.
type TaskRunner struct {
	getDatabase func(ctx context.Context) (*gorm.DB, error)

	handlers  syncx.Map[task.Type, task.Handler]
	factories syncx.Map[task.Type, task.Factory]

	// local keeps the tasks scheduled by this runner, so they can be executed
	// here even when their type has no factory.
	local syncx.Map[task.ID, localTask]

	// wake nudges idle workers when this runner schedules a task, instead of
	// waiting for the next poll.
	wake chan struct{}

	queueSize int
	errOnFull bool

	cancelFuncs syncx.Map[task.ID, context.CancelFunc]

	owner         string
	parallelism   int
	leaseDuration time.Duration
	pollInterval  time.Duration

	cleanupDelay    time.Duration
	cleanupInterval time.Duration
}

type localTask struct {
	task        task.Task
	scheduledAt time.Time
}

// Option configures optional aspects of a TaskRunner.
type Option func(r *TaskRunner)

// WithLeaseDuration sets how long a claimed task stays reserved to its runner
// without a heartbeat. A crashed runner's tasks are resumed elsewhere once it
// elapses; the heartbeat extends it every third of its duration.
func WithLeaseDuration(d time.Duration) Option {
	return func(r *TaskRunner) {
		if d > 0 {
			r.leaseDuration = d
		}
	}
}

// WithPollInterval sets how often idle workers look for tasks scheduled by
// other runners, and how often expired leases are reclaimed.
func WithPollInterval(d time.Duration) Option {
	return func(r *TaskRunner) {
		if d > 0 {
			r.pollInterval = d
		}
	}
}

// WithOwner sets the identifier this runner writes in the leases it takes.
// It must be unique among the runners sharing the database; the default
// combines the host name, the process id and a random suffix.
func WithOwner(owner string) Option {
	return func(r *TaskRunner) {
		if owner != "" {
			r.owner = owner
		}
	}
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

// ScheduleTask implements [task.Runner]. The task is persisted as pending and
// executed by whichever runner sharing the database claims it first.
func (r *TaskRunner) ScheduleTask(ctx context.Context, tsk task.Task) error {
	payload, err := tsk.MarshalJSON()
	if err != nil {
		return errors.Wrap(err, "could not marshal task payload")
	}

	now := time.Now()

	row := &Task{
		ID:          string(tsk.ID()),
		Type:        string(tsk.Type()),
		Payload:     payload,
		Status:      task.StatusPending,
		ScheduledAt: now,
	}

	if r.errOnFull {
		pending, err := r.countPending(ctx)
		if err != nil {
			return errors.WithStack(err)
		}

		if pending >= int64(r.queueSize) {
			row.Status = task.StatusFailed
			row.FinishedAt = &now
			row.Error = task.ErrQueueFull.Error()
			if err := r.save(ctx, row); err != nil {
				slog.ErrorContext(ctx, "could not persist queue-full task state", slog.Any("error", errors.WithStack(err)))
			}
			return errors.WithStack(task.ErrQueueFull)
		}
	}

	if err := r.save(ctx, row); err != nil {
		return errors.WithStack(err)
	}

	r.local.Store(tsk.ID(), localTask{task: tsk, scheduledAt: now})

	select {
	case r.wake <- struct{}{}:
	default:
	}

	return nil
}

// Run implements [task.Runner]. It serves the shared queue with a fixed worker
// pool until the context is canceled, reclaiming expired leases along the
// way. On shutdown it stops claiming and lets in-flight tasks finish before
// returning.
func (r *TaskRunner) Run(ctx context.Context) error {
	if err := r.reclaimExpired(ctx); err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "could not reclaim expired tasks", slog.Any("error", errors.WithStack(err)))
	}

	var wg sync.WaitGroup
	for i := 0; i < r.parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.work(ctx)
		}()
	}

	go r.maintain(ctx)

	<-ctx.Done()
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// work claims and executes tasks until the context is canceled, sleeping
// between polls when the queue is empty.
func (r *TaskRunner) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		claimed, err := r.claimNext(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "could not claim task", slog.Any("error", errors.WithStack(err)))
		}

		if claimed != nil {
			r.executeTask(claimed)
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-time.After(r.pollInterval):
		}
	}
}

// maintain periodically reclaims expired leases and purges old tasks.
func (r *TaskRunner) maintain(ctx context.Context) {
	reclaim := time.NewTicker(r.pollInterval)
	defer reclaim.Stop()

	cleanup := time.NewTicker(r.cleanupInterval)
	defer cleanup.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reclaim.C:
			if err := r.reclaimExpired(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "could not reclaim expired tasks", slog.Any("error", errors.WithStack(err)))
			}
		case <-cleanup.C:
			if err := r.cleanupExpired(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "could not clean up expired tasks", slog.Any("error", errors.WithStack(err)))
			}
		}
	}
}

// reclaimExpired puts back to pending every running task whose lease has
// expired. A running task without a lease was left by a runner predating
// leases and is reclaimed as well.
func (r *TaskRunner) reclaimExpired(ctx context.Context) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	res := db.WithContext(ctx).Model(&Task{}).
		Where("status = ? AND (lease_until IS NULL OR lease_until < ?)", task.StatusRunning, time.Now().UnixMilli()).
		Updates(map[string]any{"status": task.StatusPending, "lease_owner": "", "lease_until": nil})
	if res.Error != nil {
		return errors.WithStack(res.Error)
	}
	if res.RowsAffected > 0 {
		slog.InfoContext(ctx, "reclaimed tasks with an expired lease", slog.Int64("count", res.RowsAffected))
	}

	return nil
}

// claimNext claims the oldest pending task this runner can handle, or returns
// nil when there is none. Candidates are claimed with a conditional update, so
// runners racing on the same row never both win it.
func (r *TaskRunner) claimNext(ctx context.Context) (task.Task, error) {
	var types []string
	r.handlers.Range(func(t task.Type, _ task.Handler) bool {
		types = append(types, string(t))
		return true
	})
	if len(types) == 0 {
		return nil, nil
	}

	db, err := r.getDatabase(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var candidates []Task
	if err := db.WithContext(ctx).
		Select("id", "type", "payload").
		Where("status = ? AND type IN ?", task.StatusPending, types).
		Order("scheduled_at ASC").
		Limit(max(8, r.parallelism*2)).
		Find(&candidates).Error; err != nil {
		return nil, errors.WithStack(err)
	}

	for i := range candidates {
		row := candidates[i]

		claimed, err := r.claim(ctx, task.ID(row.ID))
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if !claimed {
			continue
		}

		tsk, err := r.rebuild(task.ID(row.ID), task.Type(row.Type), row.Payload)
		if err != nil {
			if err := r.finishOwned(ctx, task.ID(row.ID), task.StatusFailed, err); err != nil {
				slog.ErrorContext(ctx, "could not persist task state", slog.Any("error", errors.WithStack(err)))
			}
			continue
		}

		return tsk, nil
	}

	return nil, nil
}

// rebuild returns the in-memory instance of a task this runner scheduled, or
// rebuilds it from its payload with the registered factory.
func (r *TaskRunner) rebuild(id task.ID, taskType task.Type, payload []byte) (task.Task, error) {
	if local, exists := r.local.Load(id); exists {
		return local.task, nil
	}

	factory, exists := r.factories.Load(taskType)
	if !exists {
		return nil, errors.Errorf("no factory registered for task type '%s'", taskType)
	}

	tsk, err := factory(id, payload)
	if err != nil {
		return nil, errors.Wrap(err, "could not rebuild persisted task")
	}

	return tsk, nil
}

func (r *TaskRunner) executeTask(tsk task.Task) {
	taskID := tsk.ID()

	taskCtx, cancel := context.WithCancel(context.Background())
	r.cancelFuncs.Store(taskID, cancel)

	ctx := slogx.WithAttrs(taskCtx,
		slog.String("taskID", string(taskID)),
		slog.String("taskType", string(tsk.Type())),
	)

	heartbeatDone := make(chan struct{})
	go r.heartbeat(ctx, taskID, cancel, heartbeatDone)

	defer func() {
		close(heartbeatDone)
		cancel()
		r.cancelFuncs.Delete(taskID)
		r.local.Delete(taskID)

		if recovered := recover(); recovered != nil {
			err, ok := recovered.(error)
			if !ok {
				err = errors.Errorf("%+v", recovered)
			}
			slog.ErrorContext(ctx, "recovered panic while running task", slog.Any("error", errors.WithStack(err)))
			if err := r.finishOwned(context.Background(), taskID, task.StatusFailed, errors.WithStack(err)); err != nil {
				slog.ErrorContext(ctx, "could not persist recovered task state", slog.Any("error", errors.WithStack(err)))
			}
		}
	}()

	handler, exists := r.handlers.Load(tsk.Type())
	if !exists {
		if err := r.finishOwned(ctx, taskID, task.StatusFailed, errors.Errorf("no handler registered for task type '%s'", tsk.Type())); err != nil {
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
			if err := r.updateOwned(context.Background(), taskID, map[string]any{"progress": progress, "message": message}); err != nil {
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

	err := handler.Handle(taskCtx, tsk, events)

	close(events)
	eventsWg.Wait()

	// The terminal writes below are guarded by the lease: when the task was
	// canceled elsewhere, or its lease was lost and the task resumed by another
	// runner, they are no-ops.
	if errors.Is(err, task.ErrCanceled) {
		slog.DebugContext(ctx, "task was canceled")
		if err := r.finishOwned(context.Background(), taskID, task.StatusFailed, errors.WithStack(task.ErrCanceled)); err != nil {
			slog.ErrorContext(ctx, "could not persist canceled task state", slog.Any("error", errors.WithStack(err)))
		}
		return
	}

	if err != nil {
		err = errors.WithStack(err)
		slog.ErrorContext(ctx, "task failed", slog.Any("error", err))
		if err := r.finishOwned(context.Background(), taskID, task.StatusFailed, err); err != nil {
			slog.ErrorContext(ctx, "could not persist failed task state", slog.Any("error", errors.WithStack(err)))
		}
		return
	}

	slog.DebugContext(ctx, "task finished", slog.Duration("duration", time.Since(start)))
	if err := r.finishOwned(context.Background(), taskID, task.StatusSucceeded, nil); err != nil {
		slog.ErrorContext(ctx, "could not persist succeeded task state", slog.Any("error", errors.WithStack(err)))
	}
}

// heartbeat extends the lease of a running task until done is closed. When
// the lease can no longer be extended — the task was canceled by another
// runner, or its lease expired and another runner took it — the task's
// context is canceled.
func (r *TaskRunner) heartbeat(ctx context.Context, id task.ID, cancel context.CancelFunc, done <-chan struct{}) {
	ticker := time.NewTicker(r.leaseDuration / 3)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			db, err := r.getDatabase(ctx)
			if err != nil {
				slog.WarnContext(ctx, "could not extend task lease", slog.Any("error", errors.WithStack(err)))
				continue
			}

			res := db.WithContext(context.Background()).Model(&Task{}).
				Where("id = ? AND status = ? AND lease_owner = ?", string(id), task.StatusRunning, r.owner).
				Update("lease_until", r.leaseDeadline())
			if res.Error != nil {
				slog.WarnContext(ctx, "could not extend task lease", slog.Any("error", errors.WithStack(res.Error)))
				continue
			}

			if res.RowsAffected == 0 {
				slog.InfoContext(ctx, "task lease lost or task canceled, stopping it")
				cancel()
				return
			}
		}
	}
}

// CancelTask implements [task.Runner]. The task is marked failed with
// ErrCanceled; a task running in another runner is stopped by that runner's
// next heartbeat.
func (r *TaskRunner) CancelTask(ctx context.Context, id task.ID) error {
	state, err := r.GetTaskState(ctx, id)
	if err != nil {
		return errors.WithStack(err)
	}

	if state.Status != task.StatusPending && state.Status != task.StatusRunning {
		return errors.WithStack(task.ErrCanceled)
	}

	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	now := time.Now()
	if err := db.WithContext(ctx).Model(&Task{}).
		Where("id = ? AND status IN ?", string(id), []string{task.StatusPending, task.StatusRunning}).
		Updates(map[string]any{
			"status":      task.StatusFailed,
			"finished_at": &now,
			"error":       task.ErrCanceled.Error(),
			"lease_owner": "",
			"lease_until": nil,
		}).Error; err != nil {
		return errors.WithStack(err)
	}

	if cancelFn, exists := r.cancelFuncs.Load(id); exists {
		cancelFn()
	}
	r.local.Delete(id)

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

func (r *TaskRunner) leaseDeadline() int64 {
	return time.Now().Add(r.leaseDuration).UnixMilli()
}

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

func (r *TaskRunner) countPending(ctx context.Context) (int64, error) {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return 0, errors.WithStack(err)
	}

	var count int64
	if err := db.WithContext(ctx).Model(&Task{}).Where("status = ?", task.StatusPending).Count(&count).Error; err != nil {
		return 0, errors.WithStack(err)
	}

	return count, nil
}

// claim atomically transitions a task from pending to running under a lease
// owned by this runner, returning true only if this call performed the
// transition.
func (r *TaskRunner) claim(ctx context.Context, id task.ID) (bool, error) {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return false, errors.WithStack(err)
	}
	res := db.WithContext(ctx).Model(&Task{}).
		Where("id = ? AND status = ?", string(id), task.StatusPending).
		Updates(map[string]any{
			"status":      task.StatusRunning,
			"lease_owner": r.owner,
			"lease_until": r.leaseDeadline(),
		})
	if res.Error != nil {
		return false, errors.WithStack(res.Error)
	}
	return res.RowsAffected == 1, nil
}

// updateOwned updates a task only while it is running under this runner's
// lease.
func (r *TaskRunner) updateOwned(ctx context.Context, id task.ID, fields map[string]any) error {
	db, err := r.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	if err := db.WithContext(ctx).Model(&Task{}).
		Where("id = ? AND status = ? AND lease_owner = ?", string(id), task.StatusRunning, r.owner).
		Updates(fields).Error; err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// finishOwned writes the terminal state of a task this runner holds the
// lease of, and releases the lease.
func (r *TaskRunner) finishOwned(ctx context.Context, id task.ID, status task.Status, cause error) error {
	now := time.Now()
	fields := map[string]any{
		"status":      status,
		"finished_at": &now,
		"lease_owner": "",
		"lease_until": nil,
	}
	if status == task.StatusSucceeded {
		fields["progress"] = float32(1)
	}
	if cause != nil {
		fields["error"] = cause.Error()
	}
	return r.updateOwned(ctx, id, fields)
}

// updateState updates a task unconditionally.
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

	// Tasks scheduled here but run (or purged) elsewhere are never deleted by
	// executeTask: drop them once they are old enough to be gone.
	r.local.Range(func(id task.ID, local localTask) bool {
		if local.scheduledAt.Before(cutoff) {
			r.local.Delete(id)
		}
		return true
	})

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

func defaultOwner() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), xid.New().String())
}

// NewTaskRunner creates a persistent runner backed by db. The schema is migrated
// lazily on the first database access. Register the task handlers with
// RegisterTask and the deserialization factories with RegisterFactory before
// calling Run so pending tasks can be resumed.
func NewTaskRunner(db *gorm.DB, parallelism int, cleanupDelay time.Duration, cleanupInterval time.Duration, funcs ...Option) *TaskRunner {
	return NewTaskRunnerWithQueue(db, parallelism, 10_000, false, cleanupDelay, cleanupInterval, funcs...)
}

// NewTaskRunnerWithQueue is NewTaskRunner with control over the queue size and
// whether ScheduleTask fails (ErrQueueFull) when that many tasks are already
// pending.
func NewTaskRunnerWithQueue(db *gorm.DB, parallelism int, queueSize int, errOnFull bool, cleanupDelay time.Duration, cleanupInterval time.Duration, funcs ...Option) *TaskRunner {
	r := &TaskRunner{
		getDatabase:     createGetDatabase(db, &Task{}),
		parallelism:     max(parallelism, 1),
		queueSize:       queueSize,
		errOnFull:       errOnFull,
		wake:            make(chan struct{}, 1),
		owner:           defaultOwner(),
		leaseDuration:   defaultLeaseDuration,
		pollInterval:    defaultPollInterval,
		cleanupDelay:    cleanupDelay,
		cleanupInterval: cleanupInterval,
	}

	for _, fn := range funcs {
		fn(r)
	}

	return r
}

var (
	_ task.Runner           = &TaskRunner{}
	_ task.PersistentRunner = &TaskRunner{}
)
