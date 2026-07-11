package memory

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
)

func TestTaskManager(t *testing.T) {
	tr := NewTaskRunner(10, 24*time.Hour, time.Minute)

	var executed atomic.Int64

	tr.RegisterTask("dummy", task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		t.Logf("[%s] start", tsk.ID())
		events <- task.NewEvent(task.WithProgress(0.1))
		events <- task.NewEvent(task.WithProgress(0.5))
		events <- task.NewEvent(task.WithProgress(1))
		t.Logf("[%s] done", tsk.ID())
		executed.Add(1)
		return nil
	}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	total := int64(100)

	for range total {
		tsk := &dummyTask{
			id: task.NewID(),
		}
		t.Logf("Scheduling task %s", tsk.ID())
		tr.ScheduleTask(ctx, tsk)
	}

	if err := tr.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%+v", errors.WithStack(err))
	}

	t.Logf("executed: %d", executed.Load())

	if e, g := total, executed.Load(); e != g {
		t.Logf("executed: expected %d, got %d", e, g)
	}

	taskHeaders, err := tr.ListTasks(ctx)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if e, g := int(total), len(taskHeaders); e != g {
		t.Logf("len(taskHeaders): expected %d, got %d", e, g)
	}

	for _, header := range taskHeaders {
		state, err := tr.GetTaskState(ctx, header.ID)
		if err != nil {
			t.Fatalf("%+v", errors.WithStack(err))
		}

		if state.ScheduledAt.IsZero() {
			t.Errorf("task.ScheduledAt should not be zero value")
		}
	}
}

type dummyTask struct {
	id task.ID
}

// MarshalJSON implements [task.Task].
func (d *dummyTask) MarshalJSON() ([]byte, error) {
	return nil, nil
}

// UnmarshalJSON implements [task.Task].
func (d *dummyTask) UnmarshalJSON([]byte) error {
	return nil
}

// ID implements [task.Task].
func (d *dummyTask) ID() task.ID {
	return d.id
}

// Type implements [task.Task].
func (d *dummyTask) Type() task.Type {
	return "dummy"
}

var _ task.Task = &dummyTask{}
