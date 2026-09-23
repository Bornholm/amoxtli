package amoxtli

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	pgIndex "github.com/bornholm/amoxtli/index/postgres"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestPostgresSchemaIsolation checks that two Codex instances sharing one
// PostgreSQL database stay isolated when each connects with its own
// search_path: tables are created in the first schema of the path, and every
// query uses unqualified names. The extensions must live in a schema of the
// path shared by all (public here), created beforehand.
func TestPostgresSchemaIsolation(t *testing.T) {
	if testing.Short() || os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "pgvector/pgvector:pg17",
		tcpostgres.WithDatabase("amoxtli"),
		tcpostgres.WithUsername("amoxtli"),
		tcpostgres.WithPassword("amoxtli"),
		tcpostgres.BasicWaitStrategies(),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Errorf("could not terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("could not start container: %+v", errors.WithStack(err))
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("could not get connection string: %+v", errors.WithStack(err))
	}

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("could not connect: %+v", errors.WithStack(err))
	}
	t.Cleanup(admin.Close)

	for _, stmt := range []string{
		`CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`,
		`CREATE EXTENSION IF NOT EXISTS unaccent SCHEMA public`,
		`CREATE SCHEMA org_a`,
		`CREATE SCHEMA org_b`,
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %+v", stmt, errors.WithStack(err))
		}
	}

	open := func(schema string) (*Codex, *gormStore.Store) {
		t.Helper()

		scoped := dsn + "&search_path=" + url.QueryEscape(schema+",public")

		store, err := gormStore.NewPostgresStore(ctx, scoped)
		if err != nil {
			t.Fatalf("could not open store: %+v", errors.WithStack(err))
		}
		t.Cleanup(func() { store.Close() })

		pool, err := pgxpool.New(ctx, scoped)
		if err != nil {
			t.Fatalf("could not open pool: %+v", errors.WithStack(err))
		}
		t.Cleanup(pool.Close)

		codex, err := New(ctx,
			WithStore(store),
			WithIndexers(Indexer{ID: "postgres", Index: pgIndex.NewIndex(pool, nil), Weight: 1}),
			WithDisableHyDE(),
			WithDisableJudge(),
			WithPersistentTasks(t.TempDir()),
		)
		if err != nil {
			t.Fatalf("could not create codex: %+v", errors.WithStack(err))
		}
		t.Cleanup(func() { codex.Close() })

		return codex, store
	}

	codexA, _ := open("org_a")
	codexB, _ := open("org_b")

	collA, err := codexA.CreateCollection(ctx, "a")
	if err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}
	if _, err := codexB.CreateCollection(ctx, "b"); err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}

	source, _ := url.Parse("https://example.net/a.md")
	if err := codexA.IndexFileSync(ctx, collA, "a.md", strings.NewReader(testDocument), WithIndexFileSource(source)); err != nil {
		t.Fatalf("IndexFileSync: %+v", errors.WithStack(err))
	}

	resultsA, err := codexA.Search(ctx, "bourguignon")
	if err != nil {
		t.Fatalf("Search A: %+v", errors.WithStack(err))
	}
	if len(resultsA) != 1 {
		t.Fatalf("expected the document in org_a, got %d results", len(resultsA))
	}

	resultsB, err := codexB.Search(ctx, "bourguignon")
	if err != nil {
		t.Fatalf("Search B: %+v", errors.WithStack(err))
	}
	if len(resultsB) != 0 {
		t.Errorf("org_b sees documents of org_a: %v", hostsOf(resultsB))
	}

	collectionsB, err := codexB.ListCollections(ctx)
	if err != nil {
		t.Fatalf("ListCollections B: %+v", errors.WithStack(err))
	}
	if len(collectionsB) != 1 || collectionsB[0].Label() != "b" {
		t.Errorf("org_b sees collections of org_a")
	}

	var tables int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name LIKE 'amoxtli_%'`).Scan(&tables); err != nil {
		t.Fatalf("could not list tables: %+v", errors.WithStack(err))
	}
	if tables != 0 {
		t.Errorf("%d amoxtli tables leaked into the public schema", tables)
	}

	for _, schema := range []string{"org_a", "org_b"} {
		var count int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1`, schema).Scan(&count); err != nil {
			t.Fatalf("could not list tables: %+v", errors.WithStack(err))
		}
		if count == 0 {
			t.Errorf("no table created in %s", schema)
		}
	}
}
