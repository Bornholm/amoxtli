package retrieval

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
)

func rerankStore() *stubStore {
	return &stubStore{
		sections: map[model.SectionID]model.Section{
			"a": &stubSection{id: "a", content: "alpha content about cats"},
			"b": &stubSection{id: "b", content: "beta content about dogs"},
			"c": &stubSection{id: "c", content: "gamma content about birds"},
		},
	}
}

func resultForSource(host string, sectionIDs ...model.SectionID) *index.SearchResult {
	return &index.SearchResult{
		Source:   &url.URL{Scheme: "test", Host: host},
		Sections: sectionIDs,
		Score:    1,
	}
}

func TestLLMReranker_ReordersByRanking(t *testing.T) {
	// Initial order: docA(a), docB(b), docC(c). The LLM reranks c, a, b.
	results := []*index.SearchResult{
		resultForSource("docA", "a"),
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
	}

	llm := &stubLLM{response: `{"ranking": [3, 1, 2]}`}
	reranker := NewLLMReranker(llm, rerankStore(), 0)

	out, err := reranker.Rerank(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	gotOrder := []string{string(out[0].Sections[0]), string(out[1].Sections[0]), string(out[2].Sections[0])}
	wantOrder := []string{"c", "a", "b"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("reranked order = %v, want %v", gotOrder, wantOrder)
		}
	}

	// Scores must be monotonically non-increasing along the reranked order.
	if !(out[0].Score >= out[1].Score && out[1].Score >= out[2].Score) {
		t.Fatalf("scores not descending: %v %v %v", out[0].Score, out[1].Score, out[2].Score)
	}
	if out[0].SectionScores["c"] <= 0 {
		t.Fatalf("expected positive section score for top section, got %v", out[0].SectionScores["c"])
	}
}

func TestLLMReranker_UnrankedGoToTail(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docA", "a"),
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
	}

	// The LLM only ranks "b"; a and c are unranked and must keep their relative
	// order at the tail.
	llm := &stubLLM{response: `{"ranking": [2]}`}
	reranker := NewLLMReranker(llm, rerankStore(), 0)

	out, err := reranker.Rerank(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if out[0].Sections[0] != "b" {
		t.Fatalf("expected 'b' first, got %q", out[0].Sections[0])
	}
	if out[1].Sections[0] != "a" || out[2].Sections[0] != "c" {
		t.Fatalf("unranked tail order changed: %q %q", out[1].Sections[0], out[2].Sections[0])
	}
}

func TestLLMReranker_SingleResultShortCircuits(t *testing.T) {
	results := []*index.SearchResult{resultForSource("docA", "a")}
	llm := &stubLLM{response: `{"ranking": []}`}
	reranker := NewLLMReranker(llm, rerankStore(), 0)

	out, err := reranker.Rerank(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("expected no LLM call for a single result, got %d", llm.calls)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 result, got %d", len(out))
	}
}

// failingLLM fails every chat completion.
type failingLLM struct {
	llm.Client
	calls int
}

func (m *failingLLM) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	m.calls++
	return nil, errors.New("upstream unavailable")
}

func TestLLMReranker_KeepsOrderOnFailure(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docA", "a"),
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
	}

	for name, client := range map[string]llm.Client{
		"call fails":        &failingLLM{},
		"answer unreadable": &stubLLM{response: `not json`},
		"numbers unknown":   &stubLLM{response: `{"ranking": [7, 0, -1]}`},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := NewLLMReranker(client, rerankStore(), 0).Rerank(context.Background(), "q", results)
			if err != nil {
				t.Fatalf("unexpected error: %+v", err)
			}
			for i, want := range []model.SectionID{"a", "b", "c"} {
				if out[i].Sections[0] != want {
					t.Fatalf("result %d = %q, want the fused order kept", i, out[i].Sections[0])
				}
			}
		})
	}
}

func TestLLMReranker_FailsWhenCancelled(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docA", "a"),
		resultForSource("docB", "b"),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewLLMReranker(&failingLLM{}, rerankStore(), 0).Rerank(ctx, "q", results); err == nil {
		t.Fatal("expected an error for a cancelled search")
	}
}

func TestLLMReranker_IgnoresOutOfRangeAndRepeatedNumbers(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docA", "a"),
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
	}

	llm := &stubLLM{response: `{"ranking": [9, 3, 3, 0, 1]}`}
	out, err := NewLLMReranker(llm, rerankStore(), 0).Rerank(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	for i, want := range []model.SectionID{"c", "a", "b"} {
		if out[i].Sections[0] != want {
			t.Fatalf("result %d = %q, want %q", i, out[i].Sections[0], want)
		}
	}
}
