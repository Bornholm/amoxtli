package retrieval

import (
	"context"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/ollamatest"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/provider"
	"github.com/bornholm/genai/llm/provider/openai"
	"github.com/pkg/errors"
	"github.com/testcontainers/testcontainers-go"
	tcollama "github.com/testcontainers/testcontainers-go/modules/ollama"
)

// chatModel is a small instruction model that follows the strict-JSON prompts
// used by the grounding checker and query decomposer well enough for the
// integration assertions below.
const chatModel = "qwen2.5:3b"

// newOllamaClient starts a disposable Ollama container (reusing a named volume
// as a model cache across runs), pulls chatModel and returns a genai LLM client
// pointed at it. The container is terminated via t.Cleanup.
func newOllamaClient(t *testing.T) llm.Client {
	t.Helper()

	ctx := context.Background()

	t.Logf("starting ollama container")

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

	ollamatest.EnsureModels(t, ctx, ollamaContainer, chatModel)

	connectionStr, err := ollamaContainer.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("failed to get connection string: %+v", errors.WithStack(err))
	}

	client, err := provider.Create(ctx,
		provider.WithChatCompletion(openai.Name, openai.Options{
			CommonOptions: provider.CommonOptions{
				BaseURL: connectionStr + "/v1/",
				Model:   chatModel,
			},
		}),
	)
	if err != nil {
		t.Fatalf("failed to create llm client: %+v", errors.WithStack(err))
	}

	return client
}

// requireOllama gates the integration tests behind an explicit opt-in, matching
// the convention used by the sqlitevec/postgres integration tests.
func requireOllama(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping: requires docker + ollama")
	}
	if os.Getenv("AMOXTLI_TEST_OLLAMA") == "" {
		t.Skip("set AMOXTLI_TEST_OLLAMA=1 to run (requires docker + ollama)")
	}
}

// integrationStore is the shared evidence used across the integration subtests.
func integrationStore() *stubStore {
	return &stubStore{
		sections: map[model.SectionID]model.Section{
			"france": &stubSection{
				id:      "france",
				content: "Paris is the capital of France. It sits on the Seine river in northern France.",
			},
			"germany": &stubSection{
				id:      "germany",
				content: "Berlin is the capital of Germany and its largest city.",
			},
		},
	}
}

// TestIntegration exercises the LLM-backed retrieval components against a real
// (small) model served by Ollama. A single container is shared by all subtests.
func TestIntegration(t *testing.T) {
	requireOllama(t)

	ctx := context.Background()
	client := newOllamaClient(t)
	store := integrationStore()

	t.Run("EvidenceEvaluator/Supported", func(t *testing.T) {
		evaluator := NewLLMEvidenceEvaluator(client, store, 0)

		eval, err := evaluator.Evaluate(ctx, "What is the capital of France?",
			[]*index.SearchResult{resultWith("france")},
		)
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}

		got := eval.Grounding
		t.Logf("relevant=%v verdict: status=%s score=%.2f explanation=%q", eval.Relevant, got.Status, got.Score, got.Explanation)

		if got.Status != GroundingValid {
			t.Fatalf("expected a valid verdict for well-supported evidence, got %q", got.Status)
		}
		if got.Score < 0 || got.Score > 1 {
			t.Fatalf("score out of [0,1]: %v", got.Score)
		}
		if len(eval.Relevant) == 0 {
			t.Fatal("expected the supporting document to be judged relevant")
		}
	})

	t.Run("EvidenceEvaluator/NotSupported", func(t *testing.T) {
		evaluator := NewLLMEvidenceEvaluator(client, store, 0)

		// The evidence is about Germany while the question is about France.
		eval, err := evaluator.Evaluate(ctx, "What is the capital of France?",
			[]*index.SearchResult{resultWith("germany")},
		)
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}

		got := eval.Grounding
		t.Logf("relevant=%v verdict: status=%s score=%.2f explanation=%q", eval.Relevant, got.Status, got.Score, got.Explanation)

		if got.Status == GroundingValid {
			t.Fatalf("expected an unsupported verdict for off-topic evidence, got %q", got.Status)
		}
	})

	t.Run("QueryDecomposer", func(t *testing.T) {
		decomposer := NewLLMQueryDecomposer(client, 3)

		subs, err := decomposer.Decompose(ctx, "Compare the capitals of France and Germany.")
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}

		t.Logf("sub-queries: %v", subs)

		if len(subs) == 0 {
			t.Fatal("expected at least one sub-question")
		}
		if len(subs) > 3 {
			t.Fatalf("expected at most 3 sub-questions, got %d", len(subs))
		}
	})

	t.Run("QueryReformulator", func(t *testing.T) {
		reformulator := NewLLMQueryReformulator(client)

		out, err := reformulator.Reformulate(ctx, "capital", "the query does not say which country")
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}

		t.Logf("reformulated: %q", out)

		if out == "" {
			t.Fatal("expected a non-empty reformulated query")
		}
	})

	// End-to-end orchestration: a real grounding checker drives the re-retrieval
	// loop. Round 0 surfaces off-topic evidence (Germany) for a France question;
	// the real checker must judge it insufficient, which is what triggers a
	// reformulation and a second retrieval bringing in the relevant France
	// evidence. Search and reformulation are stubbed to keep the round transition
	// deterministic; only the grounding verdicts come from the real model.
	//
	// The assertions target what the grounding *drives* — that a not-confident
	// round-0 verdict caused exactly one extra retrieval round that enlarged the
	// evidence — rather than the exact final verdict: with maxRounds=1 the loop
	// stops on the round cap, and a small model's judgement on the fused
	// (mixed-topic) evidence is too noisy to assert on.
	t.Run("Orchestrator/GroundingDrivenReRetrieval", func(t *testing.T) {
		var round int
		searchFn := func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
			round++
			if round == 1 {
				return []*index.SearchResult{resultWith("germany")}, nil
			}
			return []*index.SearchResult{resultWith("france")}, nil
		}

		reformulator := &fakeReformulator{out: "What is the capital city of France?"}
		o := NewOrchestrator(searchFn,
			WithEvidenceEvaluator(NewLLMEvidenceEvaluator(client, store, 0)),
			WithQueryReformulator(reformulator),
			WithMaxRounds(1),
		)

		result, err := o.Search(ctx, "What is the capital of France?", 5, nil)
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}

		t.Logf("rounds=%d grounding=%+v results=%d", result.Rounds, result.Grounding, len(result.Results))

		// The off-topic round-0 evidence must have been judged not confident,
		// triggering exactly one reformulation + re-retrieval.
		if result.Rounds != 1 {
			t.Fatalf("expected exactly 1 re-retrieval round, got %d", result.Rounds)
		}
		if reformulator.calls != 1 {
			t.Fatalf("expected the query to be reformulated once, got %d", reformulator.calls)
		}
		if result.Grounding == nil {
			t.Fatal("expected a grounding verdict to be produced")
		}

		// The re-retrieval must have enlarged the fused evidence with France.
		var hasFrance bool
		for _, r := range result.Results {
			for _, s := range r.Sections {
				if s == "france" {
					hasFrance = true
				}
			}
		}
		if !hasFrance {
			t.Fatal("expected the fused evidence to include the France section")
		}
	})
}
