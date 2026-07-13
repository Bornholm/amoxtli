package gorm

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"

	gormlite "github.com/ncruces/go-sqlite3/gormlite"
	gorm "gorm.io/gorm"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	// Embed the SQLite binary
	_ "github.com/ncruces/go-sqlite3/embed"
)

const testDocument = `# Title

Some introduction.

## Section A

Content of section A.

## Section B

Content of section B.
`

// TestStore runs the store conformance against SQLite (no docker required).
func TestStore(t *testing.T) {
	ctx := context.Background()

	dsn := filepath.Join(t.TempDir(), "store_test.sqlite")

	db, err := gorm.Open(gormlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("could not open database: %+v", errors.WithStack(err))
	}

	testStoreConformance(t, ctx, NewStore(db))
}

// TestStorePostgres runs the same conformance against a real PostgreSQL
// instance, validating that the gorm store is dialect-portable.
func TestStorePostgres(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()

	dsn := startPostgresContainer(t, ctx)

	store, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatalf("could not create postgres store: %+v", errors.WithStack(err))
	}

	testStoreConformance(t, ctx, store)
}

func testStoreConformance(t *testing.T, ctx context.Context, store *Store) {
	coll, err := store.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	doc, err := markdown.Parse([]byte(testDocument))
	if err != nil {
		t.Fatalf("could not parse document: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("https://example.net/test.md")
	doc.SetSource(source)
	doc.AddCollection(coll)
	doc.SetMetadata(map[string]any{"author": "william", "year": float64(2026)})

	if err := store.SaveDocuments(ctx, doc); err != nil {
		t.Fatalf("could not save document: %+v", errors.WithStack(err))
	}

	persisted, err := store.GetDocumentByID(ctx, doc.ID())
	if err != nil {
		t.Fatalf("could not retrieve document: %+v", errors.WithStack(err))
	}

	if e, g := source.String(), persisted.Source().String(); e != g {
		t.Errorf("persisted.Source(): expected %s, got %s", e, g)
	}

	// Metadata roundtrip: via the document capability and the MetadataProvider
	// lookup used by search filtering (exercises the JSON column for both the
	// SQLite and PostgreSQL dialects).
	if g := model.Metadata(persisted); g["author"] != "william" {
		t.Errorf("persisted metadata author: expected william, got %v", g["author"])
	}
	bySource, err := store.GetDocumentsMetadataBySources(ctx, []string{source.String()})
	if err != nil {
		t.Fatalf("GetDocumentsMetadataBySources: %+v", errors.WithStack(err))
	}
	if got := bySource[source.String()]; got["author"] != "william" || got["year"].(float64) != 2026 {
		t.Errorf("metadata by source: unexpected %+v", got)
	}

	if e, g := 1, len(persisted.Collections()); e != g {
		t.Errorf("len(persisted.Collections()): expected %d, got %d", e, g)
	}

	documents, total, err := store.QueryDocumentsByCollectionID(ctx, coll.ID(), ingest.QueryDocumentsOptions{})
	if err != nil {
		t.Fatalf("could not query documents: %+v", errors.WithStack(err))
	}

	if e, g := int64(1), total; e != g {
		t.Errorf("total: expected %d, got %d", e, g)
	}

	if e, g := 1, len(documents); e != g {
		t.Fatalf("len(documents): expected %d, got %d", e, g)
	}

	// Sections should exist for the persisted document (exercises Branch scan).
	sectionIDs := make(map[string]bool)
	for _, s := range persisted.Sections() {
		sectionIDs[string(s.ID())] = true
		// Branch() reads the driver-scanned Branch column.
		if len(s.Branch()) == 0 {
			t.Errorf("section %s: expected a non-empty branch", s.ID())
		}
	}
	if len(sectionIDs) == 0 {
		t.Errorf("expected persisted document to have sections")
	}

	// ListCollections (pipeline.CollectionLister)
	listed, err := store.ListCollections(ctx, nil)
	if err != nil {
		t.Fatalf("could not list collections: %+v", errors.WithStack(err))
	}
	if e, g := 1, len(listed); e != g {
		t.Errorf("len(listed): expected %d, got %d", e, g)
	}

	// Delete by source
	if err := store.DeleteDocumentBySource(ctx, source); err != nil {
		t.Fatalf("could not delete document: %+v", errors.WithStack(err))
	}

	if _, err := store.GetDocumentByID(ctx, doc.ID()); !errors.Is(err, ingest.ErrNotFound) {
		t.Errorf("expected ingest.ErrNotFound after deletion, got %v", err)
	}
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
