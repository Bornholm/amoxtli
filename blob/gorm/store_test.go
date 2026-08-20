package gorm

import (
	"bytes"
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

// Blob bytes are content too: a photo attached to a note deserves the same
// protection as the note.
func TestStore_EncryptsBlobBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "blobs.sqlite")

	db, err := gorm.Open(gormlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("could not open database: %+v", err)
	}

	store := NewStore(db, WithEncryptionKey("a-test-key-with-at-least-32-bytes!"))

	payload := []byte("BINARY-IMAGE-PAYLOAD-1234567890")
	hash, err := store.Put(ctx, "image/png", payload)
	if err != nil {
		t.Fatalf("Put: %+v", err)
	}

	data, info, err := store.Get(ctx, hash)
	if err != nil {
		t.Fatalf("Get: %+v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Errorf("Get returned %q, want the clear payload", data)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want the clear size %d", info.Size, len(payload))
	}

	sqlDB, _ := db.DB()
	_ = sqlDB.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read database file: %+v", err)
	}
	if bytes.Contains(raw, []byte("BINARY-IMAGE-PAYLOAD")) {
		t.Error("the database file carries the blob in clear")
	}
}
