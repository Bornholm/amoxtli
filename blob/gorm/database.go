package gorm

import (
	"context"
	"sync"

	"github.com/pkg/errors"
	"gorm.io/gorm"
)

// createGetDatabase defers the auto-migration to the first use of the store
// and runs it once, the same mechanism as ingest/gorm: a store built on a
// shared connection must not migrate at construction time.
func createGetDatabase(db *gorm.DB, models ...any) func(ctx context.Context) (*gorm.DB, error) {
	var (
		migrateOnce sync.Once
		migrateErr  error
	)

	return func(ctx context.Context) (*gorm.DB, error) {
		migrateOnce.Do(func() {
			if err := db.AutoMigrate(models...); err != nil {
				migrateErr = errors.WithStack(err)
			}
		})
		if migrateErr != nil {
			return nil, errors.WithStack(migrateErr)
		}

		return db, nil
	}
}
