package retrieval

import (
	"context"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
)

// stubRelevance scores a passage by the keywords it contains: "cats" makes it
// relevant, and a passage joining "cats" with "birds" is complete.
type stubRelevance struct {
	calls [][]string
}

func (m *stubRelevance) Score(ctx context.Context, query string, passages []string) ([]PassageScore, error) {
	m.calls = append(m.calls, passages)
	scores := make([]PassageScore, len(passages))
	for i, p := range passages {
		switch {
		case strings.Contains(p, "cats") && strings.Contains(p, "birds"):
			scores[i] = PassageScore{Relevant: 0.9, Complete: 0.8, Partial: 0.15}
		case strings.Contains(p, "cats"):
			scores[i] = PassageScore{Relevant: 0.9, Complete: 0.2, Partial: 0.7}
		case strings.Contains(p, "birds"):
			scores[i] = PassageScore{Relevant: 0.6, Complete: 0.1, Partial: 0.6}
		default:
			scores[i] = PassageScore{Relevant: 0.1, Complete: 0.01, Partial: 0.04}
		}
	}
	return scores, nil
}

func TestModelEvidenceEvaluator_RelevantAndVerdict(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
		resultForSource("docA", "a"),
	}

	m := &stubRelevance{}
	evaluation, err := NewModelEvidenceEvaluator(m, rerankStore()).Evaluate(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	// Relevant sections, most relevant first: a (0.9), c (0.6); b is dropped.
	if got := evaluation.Relevant; len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("relevant = %v, want [a c]", got)
	}

	// The verdict is computed on a and c joined.
	if len(m.calls) != 2 || len(m.calls[1]) != 1 || !strings.Contains(m.calls[1][0], "cats") || !strings.Contains(m.calls[1][0], "birds") {
		t.Fatalf("sufficiency call = %v, want a and c joined", m.calls)
	}
	if evaluation.Grounding.Status != GroundingValid || evaluation.Grounding.Score != 0.8 {
		t.Fatalf("grounding = %+v, want valid 0.8", evaluation.Grounding)
	}
}

func TestModelEvidenceEvaluator_PartialAndNothingRelevant(t *testing.T) {
	m := &stubRelevance{}
	e := NewModelEvidenceEvaluator(m, rerankStore())

	evaluation, err := e.Evaluate(context.Background(), "q", []*index.SearchResult{resultForSource("docA", "a"), resultForSource("docB", "b")})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if evaluation.Grounding.Status != GroundingPartial {
		t.Fatalf("status = %s, want partial", evaluation.Grounding.Status)
	}

	evaluation, err = e.Evaluate(context.Background(), "q", []*index.SearchResult{resultForSource("docB", "b")})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if len(evaluation.Relevant) != 0 || evaluation.Grounding.Status != GroundingInvalid {
		t.Fatalf("evaluation = %+v, want nothing relevant and invalid", evaluation)
	}

	evaluation, err = e.Evaluate(context.Background(), "q", nil)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if evaluation.Grounding.Status != GroundingInvalid {
		t.Fatalf("status = %s, want invalid", evaluation.Grounding.Status)
	}
}

func TestModelReranker_OrdersByRelevance(t *testing.T) {
	results := []*index.SearchResult{
		resultForSource("docB", "b"),
		resultForSource("docC", "c"),
		resultForSource("docA", "a"),
	}

	out, err := NewModelReranker(&stubRelevance{}, rerankStore(), WithRerankWeight(1)).Rerank(context.Background(), "q", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	got := []string{string(out[0].Sections[0]), string(out[1].Sections[0]), string(out[2].Sections[0])}
	if got[0] != "a" || got[1] != "c" || got[2] != "b" {
		t.Fatalf("order = %v, want [a c b]", got)
	}
}

func TestModelReranker_BlendsWithFusionOrder(t *testing.T) {
	results := func() []*index.SearchResult {
		return []*index.SearchResult{
			resultForSource("docB", "b"),
			resultForSource("docC", "c"),
			resultForSource("docA", "a"),
		}
	}

	order := func(weight float64) []string {
		out, err := NewModelReranker(&stubRelevance{}, rerankStore(), WithRerankWeight(weight)).Rerank(context.Background(), "q", results())
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}
		return []string{string(out[0].Sections[0]), string(out[1].Sections[0]), string(out[2].Sections[0])}
	}

	// Fusion order only.
	if got := order(0); got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("order(0) = %v, want [b c a]", got)
	}
	// Half and half: c = 0.5*0.6+0.5*0.5 = 0.55, b = 0.5*0.1+0.5*1 = 0.55,
	// a = 0.5*0.9+0 = 0.45; ties keep the fusion order.
	if got := order(0.5); got[0] != "b" || got[1] != "c" || got[2] != "a" {
		t.Fatalf("order(0.5) = %v, want [b c a]", got)
	}
	// Mostly the model: a = 0.72, c = 0.58, b = 0.28.
	if got := order(0.8); got[0] != "a" || got[1] != "c" || got[2] != "b" {
		t.Fatalf("order(0.8) = %v, want [a c b]", got)
	}
}
