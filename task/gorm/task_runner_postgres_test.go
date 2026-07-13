package gorm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
	gormpostgres "gorm.io/driver/postgres"
	gorm "gorm.io/gorm"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestTaskRunnerPostgres validates that the persistent runner is
// dialect-portable by running the execute + restart-resume flow against a real
// PostgreSQL instance (nullable timestamp column, bulk status update, IN clause).
func TestTaskRunnerPostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()
	dsn := startPostgresContainer(t, ctx)

	// Execute a task and assert its terminal state is persisted.
	runner := NewTaskRunner(openPostgres(t, dsn), 2, 24*time.Hour, time.Minute)

	done := make(chan task.ID, 1)
	runner.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		events <- task.NewEvent(task.WithProgress(0.5))
		done <- tsk.ID()
		return nil
	}))
	runner.RegisterFactory(dummyTaskType, dummyFactory)

	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		_ = runner.Run(runCtx)
	}()

	tsk := &dummyTask{id: task.NewID(), Name: "pg"}
	if err := runner.ScheduleTask(runCtx, tsk); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	select {
	case <-done:
	case <-runCtx.Done():
		t.Fatal("timed out waiting for the task to run")
	}

	waitForStatus(t, runner, tsk.ID(), task.StatusSucceeded)

	got, err := runner.GetTask(runCtx, tsk.ID())
	if err != nil {
		t.Fatalf("GetTask: %+v", errors.WithStack(err))
	}
	if d, ok := got.(*dummyTask); !ok || d.Name != "pg" {
		t.Errorf("GetTask: unexpected rebuilt task %+v", got)
	}

	cancel()
	<-runnerDone

	// Restart-resume: leave a pending task with a fresh runner (never run), then
	// a second runner on the same database must resume it.
	pendingRunner := NewTaskRunner(openPostgres(t, dsn), 1, 24*time.Hour, time.Minute)
	pendingRunner.RegisterFactory(dummyTaskType, dummyFactory)
	pendingTask := &dummyTask{id: task.NewID(), Name: "resume-pg"}
	if err := pendingRunner.ScheduleTask(ctx, pendingTask); err != nil {
		t.Fatalf("ScheduleTask: %+v", errors.WithStack(err))
	}

	resumeCtx, cancelResume := context.WithTimeout(ctx, 20*time.Second)
	defer cancelResume()

	resumeRunner := NewTaskRunner(openPostgres(t, dsn), 2, 24*time.Hour, time.Minute)
	resumed := make(chan task.ID, 1)
	resumeRunner.RegisterTask(dummyTaskType, task.HandlerFunc(func(ctx context.Context, tsk task.Task, events chan task.Event) error {
		resumed <- tsk.ID()
		return nil
	}))
	resumeRunner.RegisterFactory(dummyTaskType, dummyFactory)

	resumeDone := make(chan struct{})
	go func() {
		defer close(resumeDone)
		_ = resumeRunner.Run(resumeCtx)
	}()

	select {
	case got := <-resumed:
		if got != pendingTask.ID() {
			t.Errorf("resumed unexpected task: %s != %s", got, pendingTask.ID())
		}
	case <-resumeCtx.Done():
		t.Fatal("timed out waiting for the pending task to be resumed")
	}

	waitForStatus(t, resumeRunner, pendingTask.ID(), task.StatusSucceeded)

	cancelResume()
	<-resumeDone
}

func openPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("could not open postgres: %+v", errors.WithStack(err))
	}
	return db
}

func startPostgresContainer(t *testing.T, ctx context.Context) string {
	t.Helper()

	t.Logf("Starting postgres container")

	postgresContainer, err := tcpostgres.Run(ctx, "postgres:17",
		tcpostgres.WithDatabase("amoxtli"),
		tcpostgres.WithUsername("amoxtli"),
		tcpostgres.WithPassword("amoxtli"),
		tcpostgres.BasicWaitStrategies(),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(postgresContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("failed to start container: %+v", err)
	}

	connectionStr, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	return connectionStr
}
