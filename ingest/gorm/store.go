package gorm

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

type Store struct {
	db          *gorm.DB
	getDatabase func(ctx context.Context) (*gorm.DB, error)
}

func (s *Store) withRetry(ctx context.Context, withTx bool, fn func(ctx context.Context, db *gorm.DB) error, codes ...sqlite3.ErrorCode) error {
	db, err := s.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	backoff := 500 * time.Millisecond
	maxRetries := 10
	retries := 0

	for {
		var err error
		if withTx {
			err = db.Transaction(func(tx *gorm.DB) error {
				if err := fn(ctx, tx); err != nil {
					return errors.WithStack(err)
				}

				return nil
			})
		} else {
			err = fn(ctx, db)
		}

		if err != nil {
			if retries >= maxRetries {
				return errors.WithStack(err)
			}

			var sqliteErr *sqlite3.Error
			if errors.As(err, &sqliteErr) {
				if !slices.Contains(codes, sqliteErr.Code()) {
					return errors.WithStack(err)
				}

				slog.DebugContext(ctx, "transaction failed, will retry", slog.Int("retries", retries), slog.Duration("backoff", backoff), slog.Any("error", errors.WithStack(err)))

				retries++
				time.Sleep(backoff)
				backoff *= 2
				continue
			}

			return errors.WithStack(err)
		}

		return nil
	}
}

// DB returns the underlying *gorm.DB. It is intended for advanced usage such as
// sharing the connection with a persistent task runner (task/gorm).
func (s *Store) DB() *gorm.DB {
	return s.db
}

func NewStore(db *gorm.DB) *Store {
	return &Store{
		db: db,
		getDatabase: createGetDatabase(db,
			&Document{}, &Section{}, &Collection{},
		),
	}
}

// Close releases the underlying database connection. The store owns the
// connection only when it was created through NewSQLiteStore/NewPostgresStore;
// when the *gorm.DB was provided to NewStore by the caller, closing it here
// still closes that shared connection, so use it accordingly.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(sqlDB.Close())
}

var _ ingest.Store = &Store{}
var _ ingest.MetadataProvider = &Store{}
var _ io.Closer = &Store{}
