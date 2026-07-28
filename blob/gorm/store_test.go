package gorm

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/blob/testsuite"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"

	gormlite "github.com/ncruces/go-sqlite3/gormlite"

	// Embed the SQLite binary so the driver is self-contained.
	_ "github.com/ncruces/go-sqlite3/embed"
)

// TestStoreSQLite runs the conformance suite on SQLite (no docker required).
func TestStoreSQLite(t *testing.T) {
	testsuite.TestStore(t, func(t *testing.T) blob.Store {
		db, err := gorm.Open(gormlite.Open(filepath.Join(t.TempDir(), "blobs.sqlite")), &gorm.Config{})
		if err != nil {
			t.Fatalf("could not open database: %+v", errors.WithStack(err))
		}

		return NewStore(db, WithMaxBytes(testsuite.MaxBytes))
	})
}

// TestStorePostgres runs the same suite against a real PostgreSQL instance:
// the []byte column maps to BLOB on SQLite and bytea on PostgreSQL, and only a
// run on both proves the mapping is portable.
func TestStorePostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()
	dsn := startPostgresContainer(t, ctx)

	// One container for the whole suite: each case drops the table first so it
	// still starts from an empty store.
	testsuite.TestStore(t, func(t *testing.T) blob.Store {
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		if err != nil {
			t.Fatalf("could not open database: %+v", errors.WithStack(err))
		}

		if err := db.WithContext(ctx).Exec("DROP TABLE IF EXISTS blobs").Error; err != nil {
			t.Fatalf("could not reset table: %+v", errors.WithStack(err))
		}

		return NewStore(db, WithMaxBytes(testsuite.MaxBytes))
	})
}

func startPostgresContainer(t *testing.T, ctx context.Context) string {
	t.Helper()

	t.Logf("Starting postgres container")

	postgresContainer, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("amoxtli"),
		tcpostgres.WithUsername("amoxtli"),
		tcpostgres.WithPassword("amoxtli"),
		tcpostgres.BasicWaitStrategies(),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(postgresContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("failed to start container: %+v", err)
	}

	connectionStr, err := postgresContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	return connectionStr
}
