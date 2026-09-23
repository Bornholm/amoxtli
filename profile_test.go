package amoxtli

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bornholm/amoxtli/index"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// countingLLM answers every chat call with a fixed payload and counts them.
type countingLLM struct {
	answer string
	calls  atomic.Int64
}

func (c *countingLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	c.calls.Add(1)
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, c.answer),
		llm.NewChatCompletionUsage(0, 0, 0),
	), nil
}

func (c *countingLLM) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

func (c *countingLLM) Transcription(context.Context, []byte, ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *countingLLM) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	return nil, errors.New("not implemented")
}

type profiledCodex struct {
	codex     *Codex
	collID    model.CollectionID
	rerank    *countingLLM
	grounding *countingLLM
	decompose *countingLLM
}

func newProfiledCodex(t *testing.T) *profiledCodex {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	bleveIdx, err := bleveIndex.OpenOrCreate(ctx, filepath.Join(dir, "index.bleve"))
	if err != nil {
		t.Fatalf("could not open bleve index: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { bleveIdx.Close() })

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { store.Close() })

	p := &profiledCodex{
		rerank:    &countingLLM{answer: `{"ranking": []}`},
		grounding: &countingLLM{answer: `{"relevant": [], "status": "invalid", "score": 0, "explanation": "none"}`},
		decompose: &countingLLM{answer: `{"questions": []}`},
	}

	p.codex, err = New(ctx,
		WithStore(store),
		WithIndexers(Indexer{ID: "bleve", Index: bleveIdx, Weight: 1}),
		WithLLMClient(failingLLM{err: errors.New("unexpected stage call")}),
		WithStageLLMClient(StageRerank, p.rerank),
		WithStageLLMClient(StageGrounding, p.grounding),
		WithStageLLMClient(StageDecompose, p.decompose),
		WithDisableHyDE(),
		WithDisableJudge(),
		WithReranking(),
		WithLexicalReranking(retrieval.LexicalWeights{}),
		WithGroundingCheck(),
		WithGroundingMode(retrieval.GroundingDemote),
		WithQueryDecomposition(2),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { p.codex.Close() })

	p.collID, err = p.codex.CreateCollection(ctx, "test")
	if err != nil {
		t.Fatalf("CreateCollection: %+v", errors.WithStack(err))
	}

	indexSync(t, p.codex, p.collID, "https://example.net/a.md", map[string]any{"_acl": []any{"u:alice"}})
	indexSync(t, p.codex, p.collID, "https://example.net/b.md", map[string]any{"_acl": []any{"u:bob"}})

	return p
}

func (p *profiledCodex) reset() {
	p.rerank.calls.Store(0)
	p.grounding.calls.Store(0)
	p.decompose.calls.Store(0)
}

func TestSearchProfileFastRunsNoLLMStage(t *testing.T) {
	p := newProfiledCodex(t)

	page, err := p.codex.SearchPage(context.Background(), "bourguignon",
		WithSearchCollections(p.collID), WithSearchProfile(ProfileFast))
	if err != nil {
		t.Fatalf("SearchPage: %+v", errors.WithStack(err))
	}
	if len(page.Results) != 2 {
		t.Errorf("expected both documents, got %d", len(page.Results))
	}
	if page.Grounding != nil {
		t.Error("the fast profile must not evaluate grounding")
	}
	if n := p.rerank.calls.Load() + p.grounding.calls.Load(); n != 0 {
		t.Errorf("the fast profile made %d LLM calls", n)
	}
}

func TestSearchProfileBalancedReranksWithoutGrounding(t *testing.T) {
	p := newProfiledCodex(t)

	if _, err := p.codex.SearchPage(context.Background(), "bourguignon",
		WithSearchCollections(p.collID), WithSearchProfile(ProfileBalanced)); err != nil {
		t.Fatalf("SearchPage: %+v", errors.WithStack(err))
	}
	if p.rerank.calls.Load() == 0 {
		t.Error("the balanced profile must run the LLM reranker")
	}
	if p.grounding.calls.Load() != 0 {
		t.Error("the balanced profile must not run grounding")
	}
}

func TestSearchWithoutProfileRunsEveryConfiguredStage(t *testing.T) {
	p := newProfiledCodex(t)

	page, err := p.codex.SearchPage(context.Background(), "bourguignon", WithSearchCollections(p.collID))
	if err != nil {
		t.Fatalf("SearchPage: %+v", errors.WithStack(err))
	}
	if p.rerank.calls.Load() == 0 || p.grounding.calls.Load() == 0 || page.Grounding == nil {
		t.Error("without a profile, every configured stage must run")
	}
}

func TestSearchIterativeHonoursProfileAndFilter(t *testing.T) {
	p := newProfiledCodex(t)
	ctx := context.Background()

	result, err := p.codex.SearchIterative(ctx, "bourguignon",
		WithSearchCollections(p.collID),
		WithSearchFilter(index.Any("_acl", "u:alice")),
		WithSearchProfile(ProfileDeep))
	if err != nil {
		t.Fatalf("SearchIterative: %+v", errors.WithStack(err))
	}
	if len(result.Results) != 1 || result.Results[0].Source.String() != "https://example.net/a.md" {
		t.Errorf("the filter must apply to iterative searches, got %v", hostsOf(result.Results))
	}
	if p.decompose.calls.Load() == 0 || p.grounding.calls.Load() == 0 {
		t.Error("the deep profile must decompose and evaluate grounding")
	}

	p.reset()

	if _, err := p.codex.SearchIterative(ctx, "bourguignon",
		WithSearchCollections(p.collID), WithSearchProfile(ProfileFast)); err != nil {
		t.Fatalf("SearchIterative: %+v", errors.WithStack(err))
	}
	if n := p.decompose.calls.Load() + p.grounding.calls.Load() + p.rerank.calls.Load(); n != 0 {
		t.Errorf("the fast profile made %d LLM calls in an iterative search", n)
	}
}
