package retrieval

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
)

// --- extra test doubles ---------------------------------------------------

// stubSearch returns canned results per query and records the queries it was
// asked, exposing itself as a SearchFunc.
type stubSearch struct {
	byQuery map[string][]*index.SearchResult
	calls   []string
}

func (s *stubSearch) fn() SearchFunc {
	return func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
		s.calls = append(s.calls, query)
		return s.byQuery[query], nil
	}
}

// scriptedEvaluator returns a scripted sequence of verdicts (last one repeats)
// and treats every passed section as relevant, so relevance filtering is a
// no-op and the tests isolate the orchestration logic.
type scriptedEvaluator struct {
	verdicts []*GroundingResult
	calls    int
}

func (e *scriptedEvaluator) Evaluate(ctx context.Context, query string, results []*index.SearchResult) (*EvidenceEvaluation, error) {
	idx := e.calls
	if idx >= len(e.verdicts) {
		idx = len(e.verdicts) - 1
	}
	e.calls++
	return &EvidenceEvaluation{
		Relevant:  allSectionIDs(results),
		Grounding: *e.verdicts[idx],
	}, nil
}

type fakeReformulator struct {
	out   string
	calls int
}

func (f *fakeReformulator) Reformulate(ctx context.Context, query string, hint string) (string, error) {
	f.calls++
	return f.out, nil
}

type fakeDecomposer struct {
	subs  []string
	calls int
}

func (f *fakeDecomposer) Decompose(ctx context.Context, query string) ([]string, error) {
	f.calls++
	return f.subs, nil
}

func resultWith(sectionIDs ...model.SectionID) *index.SearchResult {
	return &index.SearchResult{
		Source:   &url.URL{Scheme: "test", Host: "doc"},
		Sections: sectionIDs,
	}
}

// --- fuseResults ----------------------------------------------------------

func TestFuseResults_DedupsSectionsAndDropsEmpty(t *testing.T) {
	a := []*index.SearchResult{resultWith("s1", "s2")}
	b := []*index.SearchResult{resultWith("s2", "s3"), resultWith("s1")}

	fused := fuseResults(a, b)

	// s1,s2 from a; s3 from b; the second b-result (only s1) is fully duplicate → dropped.
	got := map[model.SectionID]int{}
	for _, r := range fused {
		for _, s := range r.Sections {
			got[s]++
		}
	}

	for _, s := range []model.SectionID{"s1", "s2", "s3"} {
		if got[s] != 1 {
			t.Fatalf("section %q should appear exactly once, got %d", s, got[s])
		}
	}
	if len(fused) != 2 {
		t.Fatalf("expected 2 non-empty results, got %d", len(fused))
	}
}

// --- iterative re-retrieval -----------------------------------------------

func TestOrchestrator_IterativeReRetrieval(t *testing.T) {
	search := &stubSearch{byQuery: map[string][]*index.SearchResult{
		"q":            {resultWith("sec-1")},
		"reformulated": {resultWith("sec-2")},
	}}
	evaluator := &scriptedEvaluator{verdicts: []*GroundingResult{
		{Status: GroundingInvalid, Score: 0.1},
		{Status: GroundingValid, Score: 0.9},
	}}
	reformulator := &fakeReformulator{out: "reformulated"}

	o := NewOrchestrator(search.fn(),
		WithEvidenceEvaluator(evaluator),
		WithQueryReformulator(reformulator),
		WithMaxRounds(1),
	)

	result, err := o.Search(context.Background(), "q", 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Rounds != 1 {
		t.Fatalf("expected exactly 1 re-retrieval round, got %d", result.Rounds)
	}
	if reformulator.calls != 1 {
		t.Fatalf("expected reformulator called once, got %d", reformulator.calls)
	}
	if len(search.calls) != 2 || search.calls[0] != "q" || search.calls[1] != "reformulated" {
		t.Fatalf("expected searches [q reformulated], got %v", search.calls)
	}
	if result.Grounding == nil || result.Grounding.Status != GroundingValid {
		t.Fatalf("expected final grounding valid, got %+v", result.Grounding)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected fused evidence of 2 results, got %d", len(result.Results))
	}
}

func TestOrchestrator_StopsAfterMaxRounds(t *testing.T) {
	search := &stubSearch{byQuery: map[string][]*index.SearchResult{
		"q":     {resultWith("sec-1")},
		"again": {resultWith("sec-2")},
	}}
	evaluator := &scriptedEvaluator{verdicts: []*GroundingResult{
		{Status: GroundingInvalid, Score: 0.1, Explanation: "missing"},
	}}
	reformulator := &fakeReformulator{out: "again"}

	o := NewOrchestrator(search.fn(),
		WithEvidenceEvaluator(evaluator),
		WithQueryReformulator(reformulator),
		WithMaxRounds(1),
	)

	result, err := o.Search(context.Background(), "q", 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Rounds != 1 {
		t.Fatalf("expected 1 round before giving up, got %d", result.Rounds)
	}
	// Evidence is fused but the verdict remains not confident: the caller decides.
	if result.Grounding == nil || result.Grounding.Status != GroundingInvalid {
		t.Fatalf("expected final grounding invalid, got %+v", result.Grounding)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected fused evidence of 2 results, got %d", len(result.Results))
	}
}

// --- query decomposition --------------------------------------------------

func TestOrchestrator_Decomposition(t *testing.T) {
	search := &stubSearch{byQuery: map[string][]*index.SearchResult{
		"q":    {resultWith("sec-a")},
		"sub1": {resultWith("sec-b")},
		"sub2": {resultWith("sec-c")},
	}}
	decomposer := &fakeDecomposer{subs: []string{"sub1", "sub2"}}

	o := NewOrchestrator(search.fn(),
		WithQueryDecomposer(decomposer),
	)

	result, err := o.Search(context.Background(), "q", 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if decomposer.calls != 1 {
		t.Fatalf("expected decomposer called once, got %d", decomposer.calls)
	}
	if len(search.calls) != 3 {
		t.Fatalf("expected 3 searches (original + 2 sub-questions), got %v", search.calls)
	}
	if len(result.Results) != 3 {
		t.Fatalf("expected fused evidence of 3 results, got %d", len(result.Results))
	}
	if result.Grounding != nil {
		t.Fatalf("grounding should be nil when checker disabled, got %+v", result.Grounding)
	}
}

// --- degenerate case: no options → plain search ---------------------------

func TestOrchestrator_PlainSearch(t *testing.T) {
	search := &stubSearch{byQuery: map[string][]*index.SearchResult{
		"q": {resultWith("sec-1")},
	}}

	o := NewOrchestrator(search.fn())

	result, err := o.Search(context.Background(), "q", 5, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(search.calls) != 1 || search.calls[0] != "q" {
		t.Fatalf("expected a single plain search, got %v", search.calls)
	}
	if result.Grounding != nil {
		t.Fatalf("expected no grounding verdict, got %+v", result.Grounding)
	}
	if result.Rounds != 0 {
		t.Fatalf("expected 0 rounds, got %d", result.Rounds)
	}
	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}
}
