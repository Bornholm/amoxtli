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
// interrupted tasks to be resumed after a restart.
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
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// TableName sets an explicit, namespaced table so the runner can share a
// database with the document store without colliding.
func (Task) TableName() string {
	return "amoxtli_tasks"
}

// createGetDatabase returns an accessor that lazily migrates the task schema on
// first use, mirroring the ingest store's approach so the runner can share the
// same *gorm.DB.
func createGetDatabase(db *gorm.DB, models ...any) func(ctx context.Context) (*gorm.DB, error) {
	var (
		migrateOnce sync.Once
		migrateErr  error
	)

	return func(ctx context.Context) (*gorm.DB, error) {
		migrateOnce.Do(func() {
			if err := db.AutoMigrate(models...); err != nil {
				migrateErr = errors.WithStack(err)
				return
			}
		})
		if migrateErr != nil {
			return nil, errors.WithStack(migrateErr)
		}

		return db, nil
	}
}
