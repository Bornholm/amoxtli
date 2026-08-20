package gorm

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"time"

	"github.com/bornholm/amoxtli/crypto"
	"github.com/bornholm/amoxtli/ingest"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"gorm.io/gorm"
)

type Store struct {
	db          *gorm.DB
	getDatabase func(ctx context.Context) (*gorm.DB, error)
	// cipher, when set, seals document content at rest (see encryption.go).
	cipher *crypto.Cipher
}

// StoreOptionFunc configures a Store at construction time.
type StoreOptionFunc func(s *Store)

// WithEncryptionKey seals document content at rest with a key of at least
// 32 bytes. The envelope — identifiers, sources, metadata — stays clear:
// stores filter on it. Losing the key makes sealed content unreadable for
// good; it must be backed up apart from the data.
func WithEncryptionKey(key string) StoreOptionFunc {
	return func(s *Store) {
		cipher, err := crypto.NewCipher(key)
		if err != nil {
			// Surfacing the error lazily would let a misconfigured store
			// run in clear without anyone noticing; failing on first use
			// is the honest behaviour.
			s.getDatabase = func(ctx context.Context) (*gorm.DB, error) {
				return nil, errors.WithStack(err)
			}
			return
		}
		s.cipher = cipher
	}
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

func NewStore(db *gorm.DB, funcs ...StoreOptionFunc) *Store {
	store := &Store{
		db: db,
		getDatabase: createGetDatabase(db,
			&Document{}, &Section{}, &Collection{}, &DocumentBlob{},
		),
	}

	for _, fn := range funcs {
		fn(store)
	}

	if store.cipher != nil {
		if err := registerEncryption(db, store.cipher); err != nil {
			getDatabase := store.getDatabase
			store.getDatabase = func(ctx context.Context) (*gorm.DB, error) {
				_, _ = getDatabase(ctx)
				return nil, errors.WithStack(err)
			}
		}
	}

	return store
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
