package gorm

import (
	"context"

	"github.com/pkg/errors"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// NewPostgresStore opens a PostgreSQL-backed ingest.Store from the given DSN
// (e.g. "postgres://user:pass@host:5432/db?sslmode=disable"). The schema is
// migrated lazily on first use. The context is used to verify connectivity
// eagerly so an unreachable database fails fast.
//
// The caller does not need any extra replace directive; the PostgreSQL driver
// is pulled through this subpackage only. Wire the resulting store into the
// facade with amoxtli.WithStore.
func NewPostgresStore(ctx context.Context, dsn string) (*Store, error) {
	db, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{
		Logger: newLogger(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not open postgres database")
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, errors.Wrap(err, "could not access underlying database")
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, errors.Wrap(err, "could not reach postgres database")
	}

	return NewStore(db), nil
}
