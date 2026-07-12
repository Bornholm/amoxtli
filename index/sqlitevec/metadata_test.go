package sqlitevec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

// TestMetadataGuard verifies that an index records its identity (embeddings
// model + vector size) on first use and refuses to reopen an existing index
// with an incompatible configuration. It needs no LLM: only the lazy migration
// path is exercised. The identity is persisted to the file, so each subtest
// opens a fresh connection to the same database.
func TestMetadataGuard(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")
	ctx := context.Background()

	// migrate opens a fresh connection, builds an index with the given options
	// and triggers the lazy migration + metadata check, returning its error.
	migrate := func(t *testing.T, funcs ...OptionFunc) error {
		t.Helper()
		conn, err := sqlite3.Open(dbFile)
		if err != nil {
			t.Fatalf("failed to open database: %+v", err)
		}
		defer conn.Close()

		idx := NewIndex(conn, nil, funcs...)
		_, err = idx.getConn(ctx)
		return err
	}

	// Fresh index: identity recorded, no error.
	if err := migrate(t, WithEmbeddingsModel("model-a")); err != nil {
		t.Fatalf("unexpected error on fresh index: %+v", err)
	}

	// Same model: accepted.
	if err := migrate(t, WithEmbeddingsModel("model-a")); err != nil {
		t.Fatalf("unexpected error reopening with the same model: %+v", err)
	}

	// Different model: rejected.
	err := migrate(t, WithEmbeddingsModel("model-b"))
	if err == nil {
		t.Fatal("expected an error reopening with a different embeddings model")
	}
	if !strings.Contains(err.Error(), "embeddings_model") {
		t.Fatalf("expected an embeddings_model mismatch error, got: %v", err)
	}

	// Different vector size: rejected too.
	err = migrate(t, WithEmbeddingsModel("model-a"), WithVectorSize(DefaultVectorSize/2))
	if err == nil {
		t.Fatal("expected an error reopening with a different vector size")
	}
	if !strings.Contains(err.Error(), "vector_size") {
		t.Fatalf("expected a vector_size mismatch error, got: %v", err)
	}
}
