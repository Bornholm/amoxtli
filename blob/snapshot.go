package blob

import (
	"context"
	"encoding/gob"
	"io"

	"github.com/bornholm/amoxtli/backup"
	"github.com/pkg/errors"
)

func init() {
	gob.Register(SnapshottedBlob{})
}

// SnapshottedBlob is the serialized form of a blob in a snapshot.
type SnapshottedBlob struct {
	Hash     string
	MimeType string
	Data     []byte
}

// Snapshotter turns any Store into a backup.Snapshotable. It is written
// against the interface rather than against an implementation for a concrete
// reason: a snapshot taken from a filesystem store restores into a database
// store and vice versa, which is what makes migrating a workspace between
// backends possible. For an all-PostgreSQL deployment the server's own SQL
// backup already covers the blobs table; this snapshot remains useful for
// portability.
type Snapshotter struct {
	store Store
}

// NewSnapshotter adapts store to the backup interfaces.
func NewSnapshotter(store Store) *Snapshotter {
	return &Snapshotter{store: store}
}

// GenerateSnapshot implements backup.Snapshotable. Blobs are streamed one at a
// time: a corpus of images does not fit in memory, and neither should its
// snapshot.
func (s *Snapshotter) GenerateSnapshot(ctx context.Context) (io.ReadCloser, error) {
	r, w := io.Pipe()

	go func() {
		defer w.Close()

		encoder := gob.NewEncoder(w)

		err := s.store.List(ctx, func(info Info) error {
			data, _, err := s.store.Get(ctx, info.Hash)
			if err != nil {
				// A blob deleted by a concurrent cleanup is not a failure of
				// the snapshot.
				if errors.Is(err, ErrNotFound) {
					return nil
				}

				return errors.WithStack(err)
			}

			return encoder.Encode(SnapshottedBlob{
				Hash:     string(info.Hash),
				MimeType: info.MimeType,
				Data:     data,
			})
		})
		if err != nil {
			w.CloseWithError(errors.WithStack(err))
		}
	}()

	return r, nil
}

// RestoreSnapshot implements backup.Snapshotable. Restoring is additive: blobs
// are content-addressed, so re-putting an existing one is a no-op and nothing
// already stored is destroyed.
func (s *Snapshotter) RestoreSnapshot(ctx context.Context, r io.Reader) error {
	decoder := gob.NewDecoder(r)

	for {
		if err := ctx.Err(); err != nil {
			return errors.WithStack(err)
		}

		var entry SnapshottedBlob

		if err := decoder.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}

			return errors.WithStack(err)
		}

		hash, err := s.store.Put(ctx, entry.MimeType, entry.Data)
		if err != nil {
			return errors.WithStack(err)
		}

		// The hash is derived from the content, so a mismatch means the
		// snapshot is corrupted: restoring it would silently break every
		// document referencing that blob.
		if string(hash) != entry.Hash {
			return errors.Errorf("blob: snapshot entry '%s' hashes to '%s'", entry.Hash, hash)
		}
	}
}

var _ backup.Snapshotable = &Snapshotter{}
