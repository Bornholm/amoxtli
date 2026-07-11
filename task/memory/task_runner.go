package memory

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/internal/syncx"
	"github.com/bornholm/amoxtli/task"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

type taskEntry struct {
	Task  task.Task
	State task.State
}

type queuedTask struct {
	task   task.Task
	ctx    context.Context
	cancel context.CancelFunc
}

type TaskRunner struct {
	runningMutex *sync.Mutex
	runningCond  sync.Cond
	running      bool

	tasks      syncx.Map[task.ID, taskEntry]
	stateMutex sync.Mutex

	handlers  syncx.Map[task.Type, task.Handler]
	queue     chan queuedTask
	errOnFull bool

	cancelFuncs syncx.Map[task.ID, context.CancelFunc]

	parallelism     int
	cleanupDelay    time.Duration
	cleanupInterval time.Duration
}

// CancelTask implements [task.Runner].
func (r *TaskRunner) CancelTask(ctx context.Context, id task.ID) error {
	entry, exists := r.tasks.Load(id)
	if !exists {
		return errors.WithStack(task.ErrNotFound)
	}

	if entry.State.Status != task.StatusPending && entry.State.Status != task.StatusRunning {
		return errors.WithStack(task.ErrCanceled)
	}

	cancelFn, exists := r.cancelFuncs.Load(id)
	if !exists {
		return errors.WithStack(task.ErrCanceled)
	}

	cancelFn()

	r.updateState(entry.Task, func(s *task.State) {
		s.Error = errors.WithStack(task.ErrCanceled)
		s.Status = task.StatusFailed
		s.FinishedAt = time.Now()
	})

	r.cancelFuncs.Delete(id)

	return nil
}

// GetTask implements [task.Runner].
func (r *TaskRunner) GetTask(ctx context.Context, id task.ID) (task.Task, error) {
	entry, exists := r.tasks.Load(id)
	if !exists {
		return nil, errors.WithStack(task.ErrNotFound)
	}
	return entry.Task, nil
}

// Run implements task.Runner.
func (r *TaskRunner) Run(ctx context.Context) error {
	r.runningMutex.Lock()
	r.running = true
	r.runningCond.Broadcast()
	r.runningMutex.Unlock()

	// Start fixed worker pool
	var wg sync.WaitGroup
	for i := 0; i < r.parallelism; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.runningMutex.Lock()
			for !r.running {
				r.runningCond.Wait()
			}
			r.runningMutex.Unlock()

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

	// Cleanup goroutine
	go func() {
		ticker := time.NewTicker(r.cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				slog.DebugContext(ctx, "running task cleaner")

				var idsToDelete []task.ID
				r.tasks.Range(func(id task.ID, entry taskEntry) bool {
					if entry.State.FinishedAt.IsZero() || !time.Now().After(entry.State.FinishedAt.Add(r.cleanupDelay)) {
						return true
					}
					idsToDelete = append(idsToDelete, id)
					return true
				})

				for _, id := range idsToDelete {
					slog.DebugContext(ctx, "deleting expired task", slog.String("taskID", string(id)))
					r.tasks.Delete(id)
					r.cancelFuncs.Delete(id)
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
			r.updateState(tsk, func(s *task.State) {
				s.Error = errors.WithStack(err)
				s.Status = task.StatusFailed
				s.FinishedAt = time.Now()
			})
		}
	}()

	handler, exists := r.handlers.Load(tsk.Type())
	if !exists {
		r.updateState(tsk, func(s *task.State) {
			s.Error = errors.Errorf("no handler registered for task type '%s'", tsk.Type())
			s.Status = task.StatusFailed
			s.FinishedAt = time.Now()
		})
		return
	}

	r.updateState(tsk, func(s *task.State) {
		s.Status = task.StatusRunning
	})

	events := make(chan task.Event, 100)

	var eventsWg sync.WaitGroup
	eventsWg.Add(1)
	go func() {
		defer eventsWg.Done()
		for e := range events {
			r.updateState(tsk, func(s *task.State) {
				if e.Progress != nil {
					s.Progress = float32(max(min(*e.Progress, 1), 0))
				}
				if e.Message != nil {
					s.Message = *e.Message
				}
			})
		}
	}()

	start := time.Now()

	err := handler.Handle(taskCtx, tsk, events)

	if errors.Is(err, task.ErrCanceled) {
		slog.DebugContext(ctx, "task was canceled")
		r.updateState(tsk, func(s *task.State) {
			s.Error = errors.WithStack(task.ErrCanceled)
			s.Status = task.StatusFailed
			s.FinishedAt = time.Now()
		})
		close(events)
		eventsWg.Wait()
		return
	}

	close(events)
	eventsWg.Wait()

	if err != nil {
		err = errors.WithStack(err)
		slog.ErrorContext(ctx, "task failed", slog.Any("error", err))
		r.updateState(tsk, func(s *task.State) {
			s.Error = err
			s.Status = task.StatusFailed
			s.FinishedAt = time.Now()
		})
		return
	}

	slog.DebugContext(ctx, "task finished", slog.Duration("duration", time.Since(start)))
	r.updateState(tsk, func(s *task.State) {
		s.Status = task.StatusSucceeded
		s.FinishedAt = time.Now()
		s.Progress = 1
	})
}

// ListTasks implements task.Runner.
func (r *TaskRunner) ListTasks(ctx context.Context) ([]task.StateHeader, error) {
	headers := make([]task.StateHeader, 0)
	r.tasks.Range(func(id task.ID, entry taskEntry) bool {
		headers = append(headers, entry.State.StateHeader)
		return true
	})
	return headers, nil
}

// RegisterTask implements task.Runner.
func (r *TaskRunner) RegisterTask(taskType task.Type, handler task.Handler) {
	r.handlers.Store(taskType, handler)
}

// ScheduleTask implements task.Runner.
func (r *TaskRunner) ScheduleTask(ctx context.Context, tsk task.Task) error {
	taskID := tsk.ID()

	ctx = slogx.WithAttrs(ctx,
		slog.String("taskID", string(taskID)),
		slog.String("taskType", string(tsk.Type())),
	)

	r.updateState(tsk, func(s *task.State) {
		s.ID = taskID
		s.ScheduledAt = time.Now()
		s.Status = task.StatusPending
		s.Type = tsk.Type()
	})

	taskCtx, cancelFn := context.WithCancel(context.Background())
	r.cancelFuncs.Store(taskID, cancelFn)

	qt := queuedTask{task: tsk, ctx: taskCtx, cancel: cancelFn}

	if r.errOnFull {
		select {
		case r.queue <- qt:
		default:
			cancelFn()
			r.cancelFuncs.Delete(taskID)
			r.updateState(tsk, func(s *task.State) {
				s.Error = errors.WithStack(task.ErrQueueFull)
				s.Status = task.StatusFailed
				s.FinishedAt = time.Now()
			})
			return errors.WithStack(task.ErrQueueFull)
		}
	} else {
		r.queue <- qt
	}

	return nil
}

func (r *TaskRunner) updateState(tsk task.Task, fn func(s *task.State)) {
	r.stateMutex.Lock()
	defer r.stateMutex.Unlock()

	entry, _ := r.tasks.LoadOrStore(tsk.ID(), taskEntry{
		Task: tsk,
		State: task.State{
			StateHeader: task.StateHeader{
				ID: tsk.ID(),
			},
		},
	})

	fn(&entry.State)

	r.tasks.Store(tsk.ID(), entry)
}

// GetTaskState implements task.Runner.
func (r *TaskRunner) GetTaskState(ctx context.Context, id task.ID) (*task.State, error) {
	entry, exists := r.tasks.Load(id)
	if !exists {
		return nil, errors.WithStack(task.ErrNotFound)
	}
	return &entry.State, nil
}

func NewTaskRunner(parallelism int, cleanupDelay time.Duration, cleanupInterval time.Duration) *TaskRunner {
	return NewTaskRunnerWithQueue(parallelism, 10_000, false, cleanupDelay, cleanupInterval)
}

func NewTaskRunnerWithQueue(parallelism int, queueSize int, errOnFull bool, cleanupDelay time.Duration, cleanupInterval time.Duration) *TaskRunner {
	runningMutex := &sync.Mutex{}
	return &TaskRunner{
		runningMutex:    runningMutex,
		runningCond:     *sync.NewCond(runningMutex),
		running:         false,
		parallelism:     parallelism,
		queue:           make(chan queuedTask, queueSize),
		errOnFull:       errOnFull,
		tasks:           syncx.Map[task.ID, taskEntry]{},
		handlers:        syncx.Map[task.Type, task.Handler]{},
		cancelFuncs:     syncx.Map[task.ID, context.CancelFunc]{},
		cleanupDelay:    cleanupDelay,
		cleanupInterval: cleanupInterval,
	}
}

var _ task.Runner = &TaskRunner{}
