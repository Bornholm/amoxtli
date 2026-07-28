// Package fs implements a filesystem-backed blob store, for the local
// (SQLite) workspaces: content under <dir>/<2 hex>/<hash>, its media type in a
// <hash>.json sidecar. Keeping the bytes out of the SQLite file avoids
// inflating a database that bleve and sqlite-vec already sit next to.
package fs

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/bornholm/amoxtli/blob"
	"github.com/pkg/errors"
)

// sidecarSuffix names the metadata file accompanying a stored blob. The media
// type cannot be re-sniffed from the content reliably enough, so it is
// persisted next to it.
const sidecarSuffix = ".json"

// Store is a filesystem blob store rooted at a directory.
type Store struct {
	dir      string
	maxBytes int64
}

// Option configures a Store.
type Option func(*Store)

// WithMaxBytes bounds the size of a single blob; <= 0 keeps the default
// (blob.DefaultMaxBytes).
func WithMaxBytes(maxBytes int64) Option {
	return func(s *Store) {
		if maxBytes > 0 {
			s.maxBytes = maxBytes
		}
	}
}

// NewStore opens (creating it if needed) a blob store rooted at dir.
func NewStore(dir string, funcs ...Option) (*Store, error) {
	if dir == "" {
		return nil, errors.New("blob/fs: directory must not be empty")
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, errors.WithStack(err)
	}

	store := &Store{dir: dir, maxBytes: blob.DefaultMaxBytes}

	for _, fn := range funcs {
		fn(store)
	}

	return store, nil
}

// sidecar is the persisted form of a blob's metadata.
type sidecar struct {
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
}

// Put implements blob.Store.
func (s *Store) Put(ctx context.Context, mimeType string, data []byte) (blob.Hash, error) {
	hash, err := blob.CheckPut(mimeType, data, s.maxBytes)
	if err != nil {
		return "", err
	}

	path := s.path(hash)

	// Content-addressed: identical bytes are already there, byte for byte.
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", errors.WithStack(err)
	}

	meta, err := json.Marshal(sidecar{MimeType: mimeType, Size: int64(len(data))})
	if err != nil {
		return "", errors.WithStack(err)
	}

	// The sidecar is written first: a blob without metadata would be
	// unreadable, whereas an orphaned sidecar is invisible to Get and List and
	// is overwritten by the next Put of the same content.
	if err := writeAtomically(path+sidecarSuffix, meta); err != nil {
		return "", errors.WithStack(err)
	}

	if err := writeAtomically(path, data); err != nil {
		return "", errors.WithStack(err)
	}

	return hash, nil
}

// Get implements blob.Store.
func (s *Store) Get(ctx context.Context, hash blob.Hash) ([]byte, *blob.Info, error) {
	if !hash.Valid() {
		return nil, nil, errors.Wrapf(blob.ErrNotFound, "malformed hash '%s'", hash)
	}

	path := s.path(hash)

	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, errors.WithStack(blob.ErrNotFound)
		}

		return nil, nil, errors.WithStack(err)
	}

	info, err := s.info(hash, int64(len(data)))
	if err != nil {
		return nil, nil, err
	}

	return data, info, nil
}

// Delete implements blob.Store.
func (s *Store) Delete(ctx context.Context, hashes ...blob.Hash) error {
	for _, hash := range hashes {
		if !hash.Valid() {
			continue
		}

		path := s.path(hash)

		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.WithStack(err)
		}

		if err := os.Remove(path + sidecarSuffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.WithStack(err)
		}
	}

	return nil
}

// List implements blob.Store.
func (s *Store) List(ctx context.Context, fn func(blob.Info) error) error {
	err := filepath.WalkDir(s.dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A directory vanishing under a concurrent Delete is not a failure
			// of the walk.
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return errors.WithStack(err)
		}

		if entry.IsDir() || strings.HasSuffix(entry.Name(), sidecarSuffix) {
			return nil
		}

		if err := ctx.Err(); err != nil {
			return errors.WithStack(err)
		}

		hash := blob.Hash(entry.Name())
		if !hash.Valid() {
			return nil
		}

		fileInfo, err := entry.Info()
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}

			return errors.WithStack(err)
		}

		info, err := s.info(hash, fileInfo.Size())
		if err != nil {
			// A blob whose sidecar disappeared is skipped rather than failing
			// the whole walk: the garbage collector must still be able to run.
			if errors.Is(err, blob.ErrNotFound) {
				return nil
			}

			return err
		}

		return fn(*info)
	})
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// info reads the metadata sidecar of a blob.
func (s *Store) info(hash blob.Hash, size int64) (*blob.Info, error) {
	raw, err := os.ReadFile(s.path(hash) + sidecarSuffix)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.Wrapf(blob.ErrNotFound, "missing metadata for blob '%s'", hash)
		}

		return nil, errors.WithStack(err)
	}

	var meta sidecar
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, errors.Wrapf(blob.ErrNotFound, "unreadable metadata for blob '%s'", hash)
	}

	return &blob.Info{Hash: hash, MimeType: meta.MimeType, Size: size}, nil
}

// path shards a blob on the first byte of its hash, the same layout as the LLM
// and description caches.
func (s *Store) path(hash blob.Hash) string {
	return filepath.Join(s.dir, string(hash)[:2], string(hash))
}

// writeAtomically writes through a temporary file and renames it into place,
// so a reader never observes partial content and an interrupted write leaves
// nothing behind.
func writeAtomically(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errors.WithStack(err)
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())

		return errors.WithStack(err)
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())

		return errors.WithStack(err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())

		return errors.WithStack(err)
	}

	return nil
}

var _ blob.Store = &Store{}
