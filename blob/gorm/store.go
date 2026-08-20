// Package gorm implements a database-backed blob store covering SQLite and
// PostgreSQL with the same code, on the model of ingest/gorm and task/gorm. It
// is the natural choice for an all-PostgreSQL deployment: one server to back
// up, and the blobs travel with the documents that reference them.
package gorm

import (
	"context"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/crypto"
	"github.com/pkg/errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Blob is the persisted form of a blob. The []byte column maps to BLOB on
// SQLite and bytea on PostgreSQL without any dialect-specific tag: gorm
// already picks the right type, and the conformance suite runs on both.
type Blob struct {
	Hash      string `gorm:"primaryKey;size:64"`
	MimeType  string `gorm:"size:255;not null"`
	Size      int64  `gorm:"not null"`
	Data      []byte `gorm:"not null"`
	CreatedAt int64  `gorm:"autoCreateTime;not null"`
}

// TableName pins the table name so it does not depend on gorm's pluralization
// rules.
func (Blob) TableName() string {
	return "blobs"
}

// Store is a database-backed blob store.
type Store struct {
	db          *gorm.DB
	maxBytes    int64
	getDatabase func(ctx context.Context) (*gorm.DB, error)
	// cipher, when set, seals blob bytes at rest. The hash keys stay those
	// of the clear content: they are identifiers, deduplication relies on
	// them, and revealing that two documents share an image is part of the
	// accepted envelope.
	cipher *crypto.Cipher
}

// Option configures a Store.
type Option func(*Store)

// WithEncryptionKey seals blob bytes at rest with a key of at least 32
// bytes. Use the same key as the ingest store: the images belong to the
// same corpus as the documents that reference them.
func WithEncryptionKey(key string) Option {
	return func(s *Store) {
		cipher, err := crypto.NewCipher(key)
		if err != nil {
			s.getDatabase = func(ctx context.Context) (*gorm.DB, error) {
				return nil, errors.WithStack(err)
			}
			return
		}
		s.cipher = cipher
	}
}

// WithMaxBytes bounds the size of a single blob; <= 0 keeps the default
// (blob.DefaultMaxBytes).
func WithMaxBytes(maxBytes int64) Option {
	return func(s *Store) {
		if maxBytes > 0 {
			s.maxBytes = maxBytes
		}
	}
}

// NewStore builds a blob store on an existing connection — typically the one
// already opened by ingest/gorm (see its Store.DB method), so a deployment
// keeps a single database.
func NewStore(db *gorm.DB, funcs ...Option) *Store {
	store := &Store{
		db:          db,
		maxBytes:    blob.DefaultMaxBytes,
		getDatabase: createGetDatabase(db, &Blob{}),
	}

	for _, fn := range funcs {
		fn(store)
	}

	return store
}

// DB returns the underlying *gorm.DB.
func (s *Store) DB() *gorm.DB {
	return s.db
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, mimeType string, data []byte) (blob.Hash, error) {
	hash, err := blob.CheckPut(mimeType, data, s.maxBytes)
	if err != nil {
		return "", err
	}

	db, err := s.getDatabase(ctx)
	if err != nil {
		return "", errors.WithStack(err)
	}

	// Size describes the clear content — it is what callers and quotas
	// reason about — so it is captured before sealing.
	entry := Blob{
		Hash:     string(hash),
		MimeType: mimeType,
		Size:     int64(len(data)),
		Data:     data,
	}

	if s.cipher != nil {
		sealed, err := s.cipher.Seal(data)
		if err != nil {
			return "", errors.WithStack(err)
		}
		entry.Data = sealed
	}

	// The hash *is* the content: an existing row holds the same bytes, so a
	// concurrent duplicate Put is a no-op rather than a conflict.
	if err := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&entry).Error; err != nil {
		return "", errors.WithStack(err)
	}

	return hash, nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, hash blob.Hash) ([]byte, *blob.Info, error) {
	if !hash.Valid() {
		return nil, nil, errors.Wrapf(blob.ErrNotFound, "malformed hash '%s'", hash)
	}

	db, err := s.getDatabase(ctx)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	var entry Blob

	if err := db.WithContext(ctx).First(&entry, "hash = ?", string(hash)).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, errors.WithStack(blob.ErrNotFound)
		}

		return nil, nil, errors.WithStack(err)
	}

	data := entry.Data
	if s.cipher != nil {
		clear, err := s.cipher.Open(data)
		if err != nil {
			return nil, nil, errors.WithStack(err)
		}
		data = clear
	}

	return data, &blob.Info{
		Hash:     blob.Hash(entry.Hash),
		MimeType: entry.MimeType,
		Size:     entry.Size,
	}, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, hashes ...blob.Hash) error {
	if len(hashes) == 0 {
		return nil
	}

	db, err := s.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	keys := make([]string, 0, len(hashes))
	for _, hash := range hashes {
		if hash.Valid() {
			keys = append(keys, string(hash))
		}
	}

	if len(keys) == 0 {
		return nil
	}

	if err := db.WithContext(ctx).Where("hash IN ?", keys).Delete(&Blob{}).Error; err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// listBatchSize bounds how many rows the walk holds at once. Only the metadata
// columns are selected: a full table of images must never be materialized in
// memory just to enumerate it.
const listBatchSize = 500

// List implements blob.Store.
func (s *Store) List(ctx context.Context, fn func(blob.Info) error) error {
	db, err := s.getDatabase(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	var lastHash string

	for {
		if err := ctx.Err(); err != nil {
			return errors.WithStack(err)
		}

		var entries []Blob

		query := db.WithContext(ctx).
			Select("hash", "mime_type", "size").
			Order("hash asc").
			Limit(listBatchSize)

		if lastHash != "" {
			query = query.Where("hash > ?", lastHash)
		}

		if err := query.Find(&entries).Error; err != nil {
			return errors.WithStack(err)
		}

		if len(entries) == 0 {
			return nil
		}

		for _, entry := range entries {
			err := fn(blob.Info{
				Hash:     blob.Hash(entry.Hash),
				MimeType: entry.MimeType,
				Size:     entry.Size,
			})
			if err != nil {
				return errors.WithStack(err)
			}
		}

		lastHash = entries[len(entries)-1].Hash
	}
}

var _ blob.Store = &Store{}
