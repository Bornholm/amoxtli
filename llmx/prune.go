package llmx

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// touchInterval is how stale an entry's modification time must be before a
// read refreshes it. The modification time records the last use of an entry
// — access times are unreliable (noatime, relatime) — and refreshing it at
// most once a day keeps reads from turning into writes.
const touchInterval = 24 * time.Hour

// staleTempAge is the age from which a leftover temporary file (a write
// interrupted before its rename) is removed by PruneCache.
const staleTempAge = time.Hour

// pruneMarker is the file whose modification time records the last
// PruneCacheEvery run of a cache directory.
const pruneMarker = ".last-prune"

// ReadCacheEntry reads a cache entry and records its use, for PruneCache to
// remove the least recently used entries first. Caches sharing a directory
// with CachingClient (such as the vision description cache) read their entries
// with it.
func ReadCacheEntry(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if now := time.Now(); now.Sub(info.ModTime()) > touchInterval {
		// Best-effort: an entry we could not touch is only pruned earlier.
		_ = os.Chtimes(path, now, now)
	}

	return data, nil
}

// PruneOptions bounds an on-disk cache.
type PruneOptions struct {
	// MaxAge removes the entries unused for longer than it; 0 sets no age
	// limit.
	MaxAge time.Duration
	// MaxSize bounds the total size of the entries, in bytes, removing the
	// least recently used first; 0 sets no size limit.
	MaxSize int64
}

// IsZero reports whether the options set no limit at all.
func (o PruneOptions) IsZero() bool {
	return o.MaxAge <= 0 && o.MaxSize <= 0
}

// PruneStats reports what PruneCache did.
type PruneStats struct {
	// Removed and Freed count the entries removed and their size in bytes.
	Removed int   `json:"removed"`
	Freed   int64 `json:"freed"`
	// Kept and Size count the entries left and their size in bytes.
	Kept int   `json:"kept"`
	Size int64 `json:"size"`
}

type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

// PruneCache removes from the cache rooted at dir the entries unused for longer
// than opts.MaxAge, then the least recently used ones until the cache fits in
// opts.MaxSize. An entry's last use is its modification time, refreshed by
// ReadCacheEntry. Leftover temporary files of interrupted writes are removed
// too. It is safe to run while the cache is in use: a removed entry is a mere
// miss, recomputed on the next request.
func PruneCache(dir string, opts PruneOptions) (PruneStats, error) {
	var (
		stats   PruneStats
		entries []cacheEntry
	)

	now := time.Now()

	remove := func(path string, size int64) {
		// An entry already gone (removed by a concurrent prune) is fine.
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return
		}
		stats.Removed++
		stats.Freed += size
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return errors.WithStack(err)
		}

		if d.IsDir() || d.Name() == pruneMarker {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return errors.WithStack(err)
		}

		age := now.Sub(info.ModTime())

		if strings.HasPrefix(d.Name(), ".tmp-") {
			if age > staleTempAge {
				remove(path, info.Size())
			}
			return nil
		}

		if opts.MaxAge > 0 && age > opts.MaxAge {
			remove(path, info.Size())
			return nil
		}

		entries = append(entries, cacheEntry{path: path, size: info.Size(), modTime: info.ModTime()})
		stats.Size += info.Size()

		return nil
	})
	if err != nil {
		return stats, errors.WithStack(err)
	}

	if opts.MaxSize > 0 && stats.Size > opts.MaxSize {
		slices.SortFunc(entries, func(a, b cacheEntry) int {
			return a.modTime.Compare(b.modTime)
		})

		for len(entries) > 0 && stats.Size > opts.MaxSize {
			oldest := entries[0]
			entries = entries[1:]
			remove(oldest.path, oldest.size)
			stats.Size -= oldest.size
		}
	}

	stats.Kept = len(entries)

	return stats, nil
}

// PruneCacheEvery runs PruneCache at most once per interval for a given cache
// directory, recording its runs in a marker file there, and reports whether it
// ran. Walking a large cache is not free: short-lived processes can call it on
// every exit.
func PruneCacheEvery(dir string, opts PruneOptions, interval time.Duration) (PruneStats, bool, error) {
	if opts.IsZero() {
		return PruneStats{}, false, nil
	}

	// No cache yet, nothing to prune: do not create one.
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return PruneStats{}, false, nil
		}
		return PruneStats{}, false, errors.WithStack(err)
	}

	marker := filepath.Join(dir, pruneMarker)

	if info, err := os.Stat(marker); err == nil && time.Since(info.ModTime()) < interval {
		return PruneStats{}, false, nil
	}

	// Record the run first: concurrent processes then skip it rather than all
	// walking the cache at once.
	if err := os.WriteFile(marker, nil, 0o644); err != nil {
		return PruneStats{}, false, errors.WithStack(err)
	}
	now := time.Now()
	if err := os.Chtimes(marker, now, now); err != nil {
		return PruneStats{}, false, errors.WithStack(err)
	}

	stats, err := PruneCache(dir, opts)
	if err != nil {
		return stats, true, errors.WithStack(err)
	}

	return stats, true, nil
}
