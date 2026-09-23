package gorm

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"

	gormlite "github.com/ncruces/go-sqlite3/gormlite"
	gorm "gorm.io/gorm"

	// Embed the SQLite binary so the driver is self-contained.
	_ "github.com/ncruces/go-sqlite3/embed"
)

const dummyTaskType task.Type = "dummy"

type dummyTask struct {
	id   task.ID
	Name string
}

func (d *dummyTask) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"name": d.Name})
}

func (d *dummyTask) UnmarshalJSON(data []byte) error {
	var payload map[string]string
	if err := json.Unmarshal(data, &payload); err != nil {
		return errors.WithStack(err)
	}
	d.Name = payload["name"]
	return nil
}

func (d *dummyTask) ID() task.ID     { return d.id }
func (d *dummyTask) Type() task.Type { return dummyTaskType }

func dummyFactory(id task.ID, payload []byte) (task.Task, error) {
	t := &dummyTask{id: id}
	if err := t.UnmarshalJSON(payload); err != nil {
		return nil, errors.WithStack(err)
	}
	return t, nil
}

var _ task.Task = &dummyTask{}

// openTestDB opens a fresh *gorm.DB against the given SQLite file path, so a
// "restart" can be simulated by opening a second connection to the same file.
func openTestDB(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("could not open database: %+v", errors.WithStack(err))
	}
	return db
}

func TestTaskRunnerExecutesAndPersists(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := filepath.Join(t.TempDir(), "tasks.sqlite")
	runner := NewTaskRunner(openTestDB(t, dsn), 4, 24*time.Hour, time.Minute)

	const total = 20
	var executed atomic.Int64
	done := make(chan task.ID, total)

	runner.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		events <- task.NewEvent(task.WithProgress(0.5), task.WithMessage("halfway"))
		executed.Add(1)
		done <- tsk.ID()
		return nil
	}))
	runner.RegisterFactory(dummyTaskType, dummyFactory)

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runner.Run(ctx)
	}()

	ids := make([]task.ID, 0, total)
	for range total {
		tsk := &dummyTask{id: task.NewID(), Name: "task"}
		if err := runner.ScheduleTask(ctx, tsk); err != nil {
			t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
		}
		ids = append(ids, tsk.ID())
	}

	for range total {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for tasks (executed %d/%d)", executed.Load(), total)
		}
	}

	// Give the terminal state writes a moment to land, then assert persistence.
	waitForStatus(t, runner, ids[0], task.StatusSucceeded)

	// A task can be enqueued both by ScheduleTask and by the startup resume; the
	// claim guard must ensure it runs exactly once.
	time.Sleep(100 * time.Millisecond)
	if got := executed.Load(); got != total {
		t.Errorf("expected exactly %d executions (no duplicates), got %d", total, got)
	}

	for _, id := range ids {
		state, err := runner.GetTaskState(ctx, id)
		if err != nil {
			t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
		}
		if state.Status != task.StatusSucceeded {
			t.Errorf("task %s: expected status succeeded, got %s", id, state.Status)
		}
		if state.Progress != 1 {
			t.Errorf("task %s: expected progress 1, got %v", id, state.Progress)
		}
	}

	headers, err := runner.ListTasks(ctx)
	if err != nil {
		t.Fatalf("ListTasks: %+v", errors.WithStack(err))
	}
	if len(headers) != total {
		t.Errorf("ListTasks: expected %d, got %d", total, len(headers))
	}

	// GetTask rebuilds the task from its persisted payload via the factory.
	got, err := runner.GetTask(ctx, ids[0])
	if err != nil {
		t.Fatalf("GetTask: %+v", errors.WithStack(err))
	}
	if d, ok := got.(*dummyTask); !ok || d.Name != "task" {
		t.Errorf("GetTask: unexpected rebuilt task %+v", got)
	}

	cancel()
	<-runnerDone
}

// TestTaskRunnerResumesPending simulates a restart: a task is persisted as
// pending by a first runner that never executes it, then a second runner opened
// on the same database resumes and runs it.
func TestTaskRunnerResumesPending(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tasks.sqlite")

	// First "process": schedule (persist pending) but never Run, so the row
	// stays pending.
	runnerA := NewTaskRunner(openTestDB(t, dsn), 2, 24*time.Hour, time.Minute)
	runnerA.RegisterFactory(dummyTaskType, dummyFactory)

	tsk := &dummyTask{id: task.NewID(), Name: "resume-me"}
	if err := runnerA.ScheduleTask(context.Background(), tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	state, err := runnerA.GetTaskState(context.Background(), tsk.ID())
	if err != nil {
		t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
	}
	if state.Status != task.StatusPending {
		t.Fatalf("expected pending before resume, got %s", state.Status)
	}

	// Second "process": a fresh runner on the same database resumes the pending
	// task.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runnerB := NewTaskRunner(openTestDB(t, dsn), 2, 24*time.Hour, time.Minute)
	resumed := make(chan task.ID, 1)
	runnerB.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		resumed <- tsk.ID()
		return nil
	}))
	runnerB.RegisterFactory(dummyTaskType, dummyFactory)

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runnerB.Run(ctx)
	}()

	select {
	case got := <-resumed:
		if got != tsk.ID() {
			t.Errorf("resumed unexpected task: %s != %s", got, tsk.ID())
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the pending task to be resumed")
	}

	waitForStatus(t, runnerB, tsk.ID(), task.StatusSucceeded)

	cancel()
	<-runnerDone
}

// TestTaskRunnerResumesInterruptedRunning simulates a crash mid-execution: a
// row left in the running state is reset to pending and re-run on the next Run.
func TestTaskRunnerResumesInterruptedRunning(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "tasks.sqlite")

	runnerA := NewTaskRunner(openTestDB(t, dsn), 2, 24*time.Hour, time.Minute)
	runnerA.RegisterFactory(dummyTaskType, dummyFactory)

	tsk := &dummyTask{id: task.NewID(), Name: "interrupted"}
	if err := runnerA.ScheduleTask(context.Background(), tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}
	// Simulate the crash: the task was picked up (running) but never finished.
	if err := runnerA.updateState(context.Background(), tsk.ID(), map[string]any{"status": task.StatusRunning}); err != nil {
		t.Fatalf("updateState: %+v", errors.WithStack(err))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	runnerB := NewTaskRunner(openTestDB(t, dsn), 2, 24*time.Hour, time.Minute)
	resumed := make(chan task.ID, 1)
	runnerB.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		resumed <- tsk.ID()
		return nil
	}))
	runnerB.RegisterFactory(dummyTaskType, dummyFactory)

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runnerB.Run(ctx)
	}()

	select {
	case got := <-resumed:
		if got != tsk.ID() {
			t.Errorf("resumed unexpected task: %s != %s", got, tsk.ID())
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the interrupted task to be resumed")
	}

	waitForStatus(t, runnerB, tsk.ID(), task.StatusSucceeded)

	cancel()
	<-runnerDone
}

func TestTaskRunnerFailedTaskPersistsError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dsn := filepath.Join(t.TempDir(), "tasks.sqlite")
	runner := NewTaskRunner(openTestDB(t, dsn), 1, 24*time.Hour, time.Minute)

	var once sync.Once
	failed := make(chan struct{})
	runner.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		once.Do(func() { close(failed) })
		return errors.New("boom")
	}))
	runner.RegisterFactory(dummyTaskType, dummyFactory)

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runner.Run(ctx)
	}()

	tsk := &dummyTask{id: task.NewID(), Name: "fail"}
	if err := runner.ScheduleTask(ctx, tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	select {
	case <-failed:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the task to run")
	}

	waitForStatus(t, runner, tsk.ID(), task.StatusFailed)

	state, err := runner.GetTaskState(ctx, tsk.ID())
	if err != nil {
		t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
	}
	if state.Error == nil || state.Error.Error() == "" {
		t.Errorf("expected a persisted error, got %v", state.Error)
	}
	if state.FinishedAt.IsZero() {
		t.Errorf("expected FinishedAt to be set on a failed task")
	}

	cancel()
	<-runnerDone
}

// waitForStatus polls the runner until the task reaches the wanted status or a
// short deadline elapses, absorbing the small delay between a handler returning
// and its terminal state being written.
func waitForStatus(t *testing.T, runner *TaskRunner, id task.ID, want task.Status) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, err := runner.GetTaskState(context.Background(), id)
		if err == nil && state.Status == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach status %s in time", id, want)
}

// openSharedDB opens a connection to a SQLite file meant to be shared by
// several runners at once: WAL and a busy timeout keep concurrent writers
// from failing with SQLITE_BUSY.
func openSharedDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	return openTestDB(t, "file:"+path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)")
}

// TestTaskRunnersShareQueue runs two runners on the same database: every task
// must run exactly once, and both runners must take part.
func TestTaskRunnersShareQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "tasks.sqlite")

	const total = 200

	var (
		mu    sync.Mutex
		runs  = map[task.ID]int{}
		byRun = map[string]int{}
		done  = make(chan struct{}, total*2)
	)

	newRunner := func(name string) *TaskRunner {
		r := NewTaskRunner(openSharedDB(t, path), 4, 24*time.Hour, time.Minute,
			WithOwner(name), WithPollInterval(20*time.Millisecond))
		r.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
			time.Sleep(time.Millisecond)
			mu.Lock()
			runs[tsk.ID()]++
			byRun[name]++
			mu.Unlock()
			done <- struct{}{}
			return nil
		}))
		r.RegisterFactory(dummyTaskType, dummyFactory)
		return r
	}

	runnerA, runnerB := newRunner("a"), newRunner("b")

	var wg sync.WaitGroup
	for _, r := range []*TaskRunner{runnerA, runnerB} {
		wg.Go(func() { _ = r.Run(ctx) })
	}

	ids := make([]task.ID, 0, total)
	for i := range total {
		scheduler := runnerA
		if i%2 == 1 {
			scheduler = runnerB
		}
		tsk := &dummyTask{id: task.NewID(), Name: "shared"}
		if err := scheduler.ScheduleTask(ctx, tsk); err != nil {
			t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
		}
		ids = append(ids, tsk.ID())
	}

	for range total {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for tasks")
		}
	}

	for _, id := range ids {
		waitForStatus(t, runnerA, id, task.StatusSucceeded)
	}

	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	for _, id := range ids {
		if runs[id] != 1 {
			t.Errorf("task %s ran %d times, expected exactly once", id, runs[id])
		}
	}
	if byRun["a"] == 0 || byRun["b"] == 0 {
		t.Errorf("expected both runners to take part, got %v", byRun)
	}

	cancel()
	wg.Wait()
}

// TestTaskRunnerReclaimsExpiredLease simulates a runner that died holding a
// task: once its lease expires, another runner resumes the task.
func TestTaskRunnerReclaimsExpiredLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "tasks.sqlite")

	dead := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute,
		WithOwner("dead"), WithLeaseDuration(300*time.Millisecond))
	dead.RegisterFactory(dummyTaskType, dummyFactory)

	tsk := &dummyTask{id: task.NewID(), Name: "orphan"}
	if err := dead.ScheduleTask(ctx, tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	// The dead runner claims the task, then never heartbeats nor finishes it.
	if claimed, err := dead.claim(ctx, tsk.ID()); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%+v", claimed, err)
	}

	resumed := make(chan task.ID, 1)
	alive := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute,
		WithOwner("alive"), WithPollInterval(50*time.Millisecond))
	alive.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		resumed <- tsk.ID()
		return nil
	}))
	alive.RegisterFactory(dummyTaskType, dummyFactory)

	started := time.Now()

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = alive.Run(ctx)
	}()

	select {
	case got := <-resumed:
		if got != tsk.ID() {
			t.Errorf("resumed unexpected task: %s != %s", got, tsk.ID())
		}
		if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
			t.Errorf("task resumed after %s, before its lease expired", elapsed)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for the orphaned task to be resumed")
	}

	waitForStatus(t, alive, tsk.ID(), task.StatusSucceeded)

	cancel()
	<-runnerDone
}

// TestTaskRunnerRemoteCancel cancels, from one runner, a task running in
// another: the running handler must see its context canceled.
func TestTaskRunnerRemoteCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	path := filepath.Join(t.TempDir(), "tasks.sqlite")

	started := make(chan struct{})
	stopped := make(chan struct{})

	worker := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute,
		WithOwner("worker"), WithLeaseDuration(150*time.Millisecond), WithPollInterval(20*time.Millisecond))
	worker.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		close(started)
		<-ctx.Done()
		close(stopped)
		return errors.WithStack(task.ErrCanceled)
	}))
	worker.RegisterFactory(dummyTaskType, dummyFactory)

	remote := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute, WithOwner("remote"))
	remote.RegisterFactory(dummyTaskType, dummyFactory)

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = worker.Run(ctx)
	}()

	tsk := &dummyTask{id: task.NewID(), Name: "long"}
	if err := remote.ScheduleTask(ctx, tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("timed out waiting for the task to start")
	}

	if err := remote.CancelTask(ctx, tsk.ID()); err != nil {
		t.Fatalf("CancelTask: %+v", errors.WithStack(err))
	}

	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("the running task was not stopped by the remote cancellation")
	}

	state, err := remote.GetTaskState(ctx, tsk.ID())
	if err != nil {
		t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
	}
	if state.Status != task.StatusFailed || state.Error == nil || state.Error.Error() != task.ErrCanceled.Error() {
		t.Errorf("expected a canceled task, got status %s error %v", state.Status, state.Error)
	}

	cancel()
	<-runnerDone
}

// TestTaskRunnerStaleWorkerCannotOverwrite checks that a runner which lost
// its lease cannot write the terminal state of a task another runner holds.
func TestTaskRunnerStaleWorkerCannotOverwrite(t *testing.T) {
	ctx := context.Background()

	path := filepath.Join(t.TempDir(), "tasks.sqlite")

	stale := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute, WithOwner("stale"))
	current := NewTaskRunner(openSharedDB(t, path), 1, 24*time.Hour, time.Minute, WithOwner("current"))

	tsk := &dummyTask{id: task.NewID(), Name: "contended"}
	if err := stale.ScheduleTask(ctx, tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	if claimed, err := stale.claim(ctx, tsk.ID()); err != nil || !claimed {
		t.Fatalf("claim: claimed=%v err=%+v", claimed, err)
	}

	// The stale lease expires and the task goes to the current runner.
	if err := stale.updateState(ctx, tsk.ID(), map[string]any{"lease_until": time.Now().Add(-time.Second).UnixMilli()}); err != nil {
		t.Fatalf("updateState: %+v", errors.WithStack(err))
	}
	if err := current.reclaimExpired(ctx); err != nil {
		t.Fatalf("reclaimExpired: %+v", errors.WithStack(err))
	}
	if claimed, err := current.claim(ctx, tsk.ID()); err != nil || !claimed {
		t.Fatalf("claim by current: claimed=%v err=%+v", claimed, err)
	}

	if err := stale.finishOwned(ctx, tsk.ID(), task.StatusFailed, errors.New("stale failure")); err != nil {
		t.Fatalf("finishOwned: %+v", errors.WithStack(err))
	}

	state, err := current.GetTaskState(ctx, tsk.ID())
	if err != nil {
		t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
	}
	if state.Status != task.StatusRunning {
		t.Fatalf("stale runner overwrote the task: status %s", state.Status)
	}

	if err := current.finishOwned(ctx, tsk.ID(), task.StatusSucceeded, nil); err != nil {
		t.Fatalf("finishOwned: %+v", errors.WithStack(err))
	}

	state, err = current.GetTaskState(ctx, tsk.ID())
	if err != nil {
		t.Fatalf("GetTaskState: %+v", errors.WithStack(err))
	}
	if state.Status != task.StatusSucceeded {
		t.Errorf("expected succeeded, got %s", state.Status)
	}
}
