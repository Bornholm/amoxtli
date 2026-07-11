package bleve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"

	bleve "github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
	"github.com/pkg/errors"
)

const mappingHashFilename = ".mapping_hash"

func mappingHash(m *mapping.IndexMappingImpl) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", errors.Wrap(err, "could not marshal mapping")
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// OpenOrCreate opens or creates a Bleve index at the given path.
// If the mapping has changed since last open, the index is recreated.
func OpenOrCreate(ctx context.Context, indexPath string) (*Index, error) {
	stat, err := os.Stat(indexPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.WithStack(err)
	}

	var bleveIdx bleve.Index

	if stat == nil {
		m := IndexMapping()

		bleveIdx, err = bleve.New(indexPath, m)
		if err != nil {
			return nil, errors.Wrap(err, "could not create bleve index")
		}

		hash, err := mappingHash(m)
		if err != nil {
			slog.WarnContext(ctx, "could not compute mapping hash", slog.Any("error", err))
		} else {
			if err := os.WriteFile(filepath.Join(indexPath, mappingHashFilename), []byte(hash), 0600); err != nil {
				slog.WarnContext(ctx, "could not store mapping hash", slog.Any("error", err))
			}
		}
	} else {
		currentMapping := IndexMapping()
		currentHash, err := mappingHash(currentMapping)
		if err != nil {
			slog.WarnContext(ctx, "could not compute current mapping hash", slog.Any("error", err))
		} else {
			hashFile := filepath.Join(indexPath, mappingHashFilename)
			storedHashBytes, err := os.ReadFile(hashFile)
			storedHash := string(storedHashBytes)
			if err != nil {
				slog.WarnContext(ctx, "could not read stored mapping hash", slog.Any("error", err))
			}

			if storedHash != currentHash {
				slog.InfoContext(ctx, "bleve index mapping has changed, recreating index",
					slog.String("path", indexPath))

				if err := os.RemoveAll(indexPath); err != nil {
					slog.WarnContext(ctx, "could not delete old bleve index, opening as-is", slog.Any("error", err))
					bleveIdx, err = bleve.Open(indexPath)
					if err != nil {
						return nil, errors.Wrap(err, "could not open bleve index")
					}
				} else {
					bleveIdx, err = bleve.New(indexPath, currentMapping)
					if err != nil {
						return nil, errors.Wrap(err, "could not create new bleve index")
					}
				}

				if err := os.WriteFile(hashFile, []byte(currentHash), 0600); err != nil {
					slog.WarnContext(ctx, "could not store mapping hash", slog.Any("error", err))
				}

				return NewIndex(bleveIdx), nil
			}
		}

		bleveIdx, err = bleve.Open(indexPath)
		if err != nil {
			return nil, errors.Wrap(err, "could not open bleve index")
		}
	}

	return NewIndex(bleveIdx), nil
}
