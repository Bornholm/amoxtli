package retrieval

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
)

// countWord counts the occurrences of the exact word w in s.
func countWord(s, w string) int {
	n := 0
	for _, f := range strings.Fields(s) {
		if f == w {
			n++
		}
	}
	return n
}

func longSectionsFixture(words int) (*stubStore, []*index.SearchResult) {
	long := strings.TrimSpace(strings.Repeat("word ", words))
	store := &stubStore{sections: map[model.SectionID]model.Section{
		"long-1": &stubSection{id: "long-1", content: long},
		"long-2": &stubSection{id: "long-2", content: long},
	}}
	results := []*index.SearchResult{
		{Source: &url.URL{Scheme: "test", Host: "doc"}, Sections: []model.SectionID{"long-1", "long-2"}},
	}
	return store, results
}

// TestEvaluatorPromptCapsSectionWords checks that each section is truncated to
// the per-section cap inside the evaluator prompt, independently of the total
// budget.
func TestEvaluatorPromptCapsSectionWords(t *testing.T) {
	store, results := longSectionsFixture(500)

	e := NewLLMEvidenceEvaluator(nil, store, 8000, WithMaxSectionWords(50))
	prompt, err := e.getUserPrompt(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := countWord(prompt, "word"), 2*50; got != want {
		t.Errorf("prompt contains %d section words, want %d (2 sections × 50)", got, want)
	}
}

// TestEvaluatorPromptDefaultSectionCap checks the default per-section cap.
func TestEvaluatorPromptDefaultSectionCap(t *testing.T) {
	store, results := longSectionsFixture(500)

	e := NewLLMEvidenceEvaluator(nil, store, 8000)
	prompt, err := e.getUserPrompt(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got, want := countWord(prompt, "word"), 2*DefaultMaxSectionWords; got != want {
		t.Errorf("prompt contains %d section words, want %d (2 sections × default cap)", got, want)
	}
}

// TestRerankerPromptCapsSectionWords checks the same truncation on the reranker
// prompt, and that the total budget still applies on top of the section cap.
func TestRerankerPromptCapsSectionWords(t *testing.T) {
	store, results := longSectionsFixture(500)

	r := NewLLMReranker(nil, store, 8000, WithMaxSectionWords(50))
	prompt, err := r.getUserPrompt(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := countWord(prompt, "word"), 2*50; got != want {
		t.Errorf("prompt contains %d section words, want %d (2 sections × 50)", got, want)
	}

	// A total budget smaller than the section cap wins.
	r = NewLLMReranker(nil, store, 30, WithMaxSectionWords(50))
	prompt, err = r.getUserPrompt(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := countWord(prompt, "word"); got > 30 {
		t.Errorf("prompt contains %d section words, want <= 30 (total budget)", got)
	}
}
