package retrieval

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
)

// --- test doubles ---------------------------------------------------------

// stubLLM implements llm.Client by embedding the interface (so only the methods
// exercised by the tests need real bodies) and returns a canned completion.
type stubLLM struct {
	llm.Client
	response string
	calls    int
}

func (m *stubLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	m.calls++
	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, m.response),
		llm.NewChatCompletionUsage(0, 0, 0),
	), nil
}

// stubSection implements model.Section by embedding the interface.
type stubSection struct {
	model.Section
	id      model.SectionID
	content string
}

func (s *stubSection) ID() model.SectionID      { return s.id }
func (s *stubSection) Content() ([]byte, error) { return []byte(s.content), nil }

// stubStore implements retrieval.SectionStore.
type stubStore struct {
	sections map[model.SectionID]model.Section
}

func (s *stubStore) GetSectionsByIDs(ctx context.Context, ids []model.SectionID) (map[model.SectionID]model.Section, error) {
	out := map[model.SectionID]model.Section{}
	for _, id := range ids {
		if sec, ok := s.sections[id]; ok {
			out[id] = sec
		}
	}
	return out, nil
}

func newStubStore() *stubStore {
	return &stubStore{
		sections: map[model.SectionID]model.Section{
			"sec-1": &stubSection{id: "sec-1", content: "Paris is the capital of France."},
		},
	}
}

func oneResult() []*index.SearchResult {
	return []*index.SearchResult{
		{Source: &url.URL{Scheme: "test", Host: "doc"}, Sections: []model.SectionID{"sec-1"}},
	}
}

// --- LLMEvidenceEvaluator -------------------------------------------------

func TestLLMEvidenceEvaluator_EmptyResults(t *testing.T) {
	evaluator := NewLLMEvidenceEvaluator(&stubLLM{}, newStubStore(), 0)

	got, err := evaluator.Evaluate(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Grounding.Status != GroundingInvalid {
		t.Fatalf("expected invalid on empty results, got %q", got.Grounding.Status)
	}
	if len(got.Relevant) != 0 {
		t.Fatalf("expected no relevant sections on empty results, got %d", len(got.Relevant))
	}
}

func TestLLMEvidenceEvaluator_ParsesRelevanceAndVerdict(t *testing.T) {
	llmClient := &stubLLM{response: `{"relevant":["sec-1"],"status":"valid","score":0.9,"explanation":"fully supported"}`}
	evaluator := NewLLMEvidenceEvaluator(llmClient, newStubStore(), 0)

	got, err := evaluator.Evaluate(context.Background(), "In which country is Paris?", oneResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Grounding.Status != GroundingValid {
		t.Fatalf("expected valid, got %q", got.Grounding.Status)
	}
	if got.Grounding.Score != 0.9 {
		t.Fatalf("expected score 0.9, got %v", got.Grounding.Score)
	}
	if len(got.Relevant) != 1 || got.Relevant[0] != "sec-1" {
		t.Fatalf("expected relevant [sec-1], got %v", got.Relevant)
	}
	if llmClient.calls != 1 {
		t.Fatalf("expected exactly 1 LLM call, got %d", llmClient.calls)
	}
}

func TestLLMEvidenceEvaluator_ClampsScoreAndNormalizesStatus(t *testing.T) {
	llmClient := &stubLLM{response: `{"relevant":[],"status":"WeIrD","score":1.7,"explanation":""}`}
	evaluator := NewLLMEvidenceEvaluator(llmClient, newStubStore(), 0)

	got, err := evaluator.Evaluate(context.Background(), "q", oneResult())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Grounding.Status != GroundingInvalid {
		t.Fatalf("unknown status should normalize to invalid, got %q", got.Grounding.Status)
	}
	if got.Grounding.Score != 1.0 {
		t.Fatalf("score should clamp to 1.0, got %v", got.Grounding.Score)
	}
}

// --- FilterRelevant -------------------------------------------------------

func TestFilterRelevant_KeepsSelectedDropsEmpty(t *testing.T) {
	results := []*index.SearchResult{resultWith("s1", "s2"), resultWith("s3")}

	filtered := FilterRelevant(results, []model.SectionID{"s1", "s3"})

	// s2 dropped from the first result; the whole second result would be empty
	// only if s3 were excluded — here s3 is kept, so both results survive.
	got := map[model.SectionID]bool{}
	for _, r := range filtered {
		for _, s := range r.Sections {
			got[s] = true
		}
	}
	if got["s2"] {
		t.Fatal("s2 should have been filtered out")
	}
	if !got["s1"] || !got["s3"] {
		t.Fatalf("expected s1 and s3 to be kept, got %v", got)
	}
}

func TestFilterRelevant_DropsFullyIrrelevantResult(t *testing.T) {
	results := []*index.SearchResult{resultWith("s1"), resultWith("s2")}

	filtered := FilterRelevant(results, []model.SectionID{"s1"})

	if len(filtered) != 1 {
		t.Fatalf("expected the s2-only result to be dropped, got %d results", len(filtered))
	}
}
