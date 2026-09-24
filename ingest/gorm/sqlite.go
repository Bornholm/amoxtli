package gorm

import (
	"net/url"
	"runtime"
	"strings"

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
func NewSQLiteStore(dsn string, funcs ...StoreOptionFunc) (*Store, error) {
	memory := isMemoryDSN(dsn)

	// The pragmas go in the DSN, applied to every connection of the pool —
	// an Exec would reach one connection only. synchronous=NORMAL is the
	// standard companion of WAL: commits no longer fsync individually (the
	// WAL is fsynced at checkpoint time), which is what makes an ingestion of
	// many documents affordable. Durability is preserved across process
	// crashes; only a power loss can cost the last transactions.
	pragmas := []string{"busy_timeout(5000)", "foreign_keys(1)"}
	if !memory {
		pragmas = append(pragmas, "journal_mode(wal)", "synchronous(normal)")
	}

	db, err := gorm.Open(gormlite.Open(withPragmas(dsn, pragmas)), &gorm.Config{
		Logger: newLogger(),
	})
	if err != nil {
		return nil, errors.Wrap(err, "could not open sqlite database")
	}

	internalDB, err := db.DB()
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// WAL lets readers run alongside the writer: a pool serves concurrent
	// searches, while writers wait for each other through busy_timeout. An
	// in-memory database is private to its connection, so it keeps one.
	if memory {
		internalDB.SetMaxOpenConns(1)
	} else {
		internalDB.SetMaxOpenConns(sqlitePoolSize)
		internalDB.SetMaxIdleConns(sqlitePoolSize)
	}

	return NewStore(db, funcs...), nil
}

// sqlitePoolSize bounds the connections of a file store.
var sqlitePoolSize = max(4, runtime.NumCPU())

func isMemoryDSN(dsn string) bool {
	return dsn == ":memory:" || strings.Contains(dsn, "mode=memory") || strings.HasPrefix(dsn, "file::memory:")
}

// withPragmas adds _pragma parameters to a DSN, turning a plain path into a
// file: URI.
func withPragmas(dsn string, pragmas []string) string {
	if dsn == ":memory:" {
		dsn = "file::memory:"
	} else if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	values := make([]string, 0, len(pragmas))
	for _, p := range pragmas {
		values = append(values, "_pragma="+url.QueryEscape(p))
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + strings.Join(values, "&")
}
