package gorm

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// Task is the persisted representation of a task.Task: its serialized payload
// (produced by the task's json.Marshaler) plus the mutable execution state. It
// is the single source of truth for the persistent runner, allowing pending and
// interrupted tasks to be resumed after a restart, and runners sharing the
// database to split the work.
type Task struct {
	ID          string `gorm:"primaryKey;autoIncrement:false"`
	Type        string `gorm:"index"`
	Payload     []byte
	Status      string `gorm:"index"`
	ScheduledAt time.Time
	FinishedAt  *time.Time
	Progress    float32
	Message     string
	Error       string
	// LeaseOwner identifies the runner holding a running task.
	LeaseOwner string `gorm:"index"`
	// LeaseUntil is the lease deadline, in Unix milliseconds: an integer rather
	// than a time, so the expiry comparison stays numeric on every backend.
	LeaseUntil *int64 `gorm:"index"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// TableName sets an explicit, namespaced table so the runner can share a
// database with the document store without colliding.
func (Task) TableName() string {
	return "amoxtli_tasks"
}

// createGetDatabase returns an accessor that lazily migrates the task schema on
// first use, mirroring the ingest store's approach so the runner can share the
// same *gorm.DB.
//
// A failed migration is not remembered: runners sharing a database may start
// together and race on creating the table, and the loser must simply retry on
// its next access rather than stay broken.
func createGetDatabase(db *gorm.DB, models ...any) func(ctx context.Context) (*gorm.DB, error) {
	var (
		mu       sync.Mutex
		migrated bool
	)

	return func(ctx context.Context) (*gorm.DB, error) {
		mu.Lock()
		defer mu.Unlock()

		if !migrated {
			if err := migrate(ctx, db, models...); err != nil {
				return nil, errors.WithStack(err)
			}
			migrated = true
		}

		return db, nil
	}
}

// migrateAttempts bounds how many times a migration racing with another
// runner's is retried before giving up.
const migrateAttempts = 5

// migrate runs the schema migration, retrying when it loses a race with a
// concurrent migration of the same schema (the table appears between the
// existence check and the creation).
func migrate(ctx context.Context, db *gorm.DB, models ...any) error {
	var err error
	for attempt := range migrateAttempts {
		if err = db.WithContext(ctx).AutoMigrate(models...); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return errors.WithStack(err)
}
