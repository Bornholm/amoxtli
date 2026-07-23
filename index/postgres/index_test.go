package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/testsuite"
	"github.com/bornholm/amoxtli/internal/ollamatest"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcollama "github.com/testcontainers/testcontainers-go/modules/ollama"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// TestIndexFullTextOnly runs the conformance suite against the full-text leg
// alone (no llm.Client): only a PostgreSQL container is required.
func TestIndexFullTextOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}

	ctx := context.Background()

	connectionStr := startPostgresContainer(t, ctx)

	testsuite.TestIndex(t, func(t *testing.T) (index.Index, error) {
		pool, err := resetDatabase(t, ctx, connectionStr)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return NewIndex(pool, nil), nil
	})
}

// TestIndexHybrid runs the conformance suite with both legs active
// (full-text + pgvector embeddings): requires PostgreSQL and Ollama.
func TestIndexHybrid(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + postgres + ollama")
	}
	if os.Getenv("AMOXTLI_TEST_POSTGRES") == "" {
		t.Skip("set AMOXTLI_TEST_POSTGRES=1 to run (requires docker + postgres)")
	}
	if os.Getenv("AMOXTLI_TEST_OLLAMA") == "" {
		t.Skip("set AMOXTLI_TEST_OLLAMA=1 to run (requires docker + ollama)")
	}

	ctx := context.Background()

	connectionStr := startPostgresContainer(t, ctx)

	embeddingsModel := "mxbai-embed-large:latest"
	client := startOllamaClient(t, ctx, embeddingsModel)

	testsuite.TestIndex(t, func(t *testing.T) (index.Index, error) {
		pool, err := resetDatabase(t, ctx, connectionStr)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return NewIndex(pool, client,
			WithEmbeddingsModel(embeddingsModel),
			WithMaxWords(500),
		), nil
	})
}

func startPostgresContainer(t *testing.T, ctx context.Context) string {
	t.Helper()

	t.Logf("Starting postgres (pgvector) container")

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

func resetDatabase(t *testing.T, ctx context.Context, connectionStr string) (*pgxpool.Pool, error) {
	t.Helper()

	pool, err := pgxpool.New(ctx, connectionStr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create connection pool")
	}

	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS amoxtli_chunk_collections, amoxtli_chunks, amoxtli_document_metadata CASCADE;`); err != nil {
		return nil, errors.Wrap(err, "failed to reset database")
	}

	return pool, nil
}

func startOllamaClient(t *testing.T, ctx context.Context, models ...string) llm.Client {
	t.Helper()

	t.Logf("Starting ollama container")

	ollamaContainer, err := tcollama.Run(ctx, "ollama/ollama:0.5.7", testcontainers.CustomizeRequest(testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Mounts: testcontainers.ContainerMounts{
				{
					Source: testcontainers.GenericVolumeMountSource{
						Name: "ollama-data",
					},
					Target: "/root/.ollama",
				},
			},
		},
	}))
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ollamaContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	})
	if err != nil {
		t.Fatalf("failed to start container: %+v", err)
	}

	ollamatest.EnsureModels(t, ctx, ollamaContainer, models...)

	connectionStr, err := ollamaContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	client, err := provider.Create(ctx,
		provider.WithEmbeddings(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   models[0],
			},
		}),
	)
	if err != nil {
		t.Fatalf("failed to create llm client: %+v", errors.WithStack(err))
	}

	return client
}
