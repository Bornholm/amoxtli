package gorm

import (
	gormlite "github.com/ncruces/go-sqlite3/gormlite"
	"github.com/pkg/errors"
	"gorm.io/gorm"

	// Embed the SQLite binary so the driver is self-contained.
	_ "github.com/ncruces/go-sqlite3/embed"
)

// NewSQLiteStore opens a SQLite-backed ingest.Store at the given DSN (a file
// path, or ":memory:"). The connection is tuned for the library's usage
// (WAL journal, foreign keys, single writer) and the schema is migrated
// lazily on first use.
//
// Wire the resulting store into the facade with amoxtli.WithStore; the caller
// owns the store and must Close it.
func NewSQLiteStore(dsn string) (*Store, error) {
	db, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{
		Logger: newLogger(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not open sqlite database")
	}

	internalDB, err := db.DB()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// SQLite allows a single writer; keep one connection to avoid "database is
	// locked" contention.
	internalDB.SetMaxOpenConns(1)

	if err := db.Exec("PRAGMA journal_mode=wal; PRAGMA foreign_keys=on; PRAGMA busy_timeout=5000").Error; err != nil {
		return nil, errors.WithStack(err)
	}

	return NewStore(db), nil
}
