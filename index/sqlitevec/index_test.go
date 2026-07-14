package sqlitevec

import (
	"context"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/index/testsuite"
	"github.com/bornholm/amoxtli/internal/ollamatest"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcollama "github.com/testcontainers/testcontainers-go/modules/ollama"
)

func TestIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: requires docker + ollama")
	}
	if os.Getenv("AMOXTLI_TEST_OLLAMA") == "" {
		t.Skip("set AMOXTLI_TEST_OLLAMA=1 to run (requires docker + ollama)")
	}

	ctx := context.Background()

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
	defer func() {
		if err := testcontainers.TerminateContainer(ollamaContainer); err != nil {
			t.Fatalf("failed to terminate container: %+v", errors.WithStack(err))
		}
	}()
	if err != nil {
		t.Fatalf("failed to start container: %+v", err)
	}

	chatCompletionModel := "qwen2.5:3b"
	embeddingsModel := "mxbai-embed-large:latest"

	ollamatest.EnsureModels(t, ctx, ollamaContainer, chatCompletionModel, embeddingsModel)

	connectionStr, err := ollamaContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	client, err := provider.Create(ctx,
		provider.WithChatCompletion(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   chatCompletionModel,
			},
		}),
		provider.WithEmbeddings(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   embeddingsModel,
			},
		}),
	)
	if err != nil {
		t.Fatalf("failed to create llm client: %+v", errors.WithStack(err))
	}

	testsuite.TestIndex(t, func(t *testing.T) (index.Index, error) {
		dbFile := "./testdata/test_index.sqlite"

		if err := os.RemoveAll(dbFile); err != nil {
			return nil, errors.WithStack(err)
		}

		// Also remove WAL/SHM files left over from previous test runs
		walFile := dbFile + "-wal"
		shmFile := dbFile + "-shm"
		_ = os.RemoveAll(walFile)
		_ = os.RemoveAll(shmFile)

		db, err := sqlite3.Open(dbFile)
		if err != nil {
			return nil, errors.Wrap(err, "failed to open sqlite database")
		}

		index := NewIndex(db, client,
			WithEmbeddingsModel("mxbai-embed-large:latest"),
			WithMaxWords(500),
		)

		return index, nil
	})
}
