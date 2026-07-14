package eval

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/index"
)

func TestLoadDataset(t *testing.T) {
	ds, err := LoadDataset("testdata/recipes.json")
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}
	if ds.Name != "recipes-golden" {
		t.Errorf("Name = %q, want recipes-golden", ds.Name)
	}
	if len(ds.Queries) != 5 {
		t.Fatalf("len(Queries) = %d, want 5", len(ds.Queries))
	}
	if ds.Queries[0].ID != "air-fryer-temp" {
		t.Errorf("Queries[0].ID = %q", ds.Queries[0].ID)
	}
	if got := ds.Queries[0].RelevantSources; len(got) != 1 || got[0] != "mem://recipes/air-fryer-mixed-vegetables" {
		t.Errorf("unexpected RelevantSources: %v", got)
	}
}

// TestEvaluateWithStub scores a deterministic retriever against the fixture: it
// always returns the golden target at a fixed rank so the aggregate metrics are
// predictable.
func TestEvaluateWithStub(t *testing.T) {
	ds, err := LoadDataset("testdata/recipes.json")
	if err != nil {
		t.Fatalf("LoadDataset: %v", err)
	}

	// Retriever that places each query's first relevant source at rank 2.
	byQuery := make(map[string]string, len(ds.Queries))
	for _, q := range ds.Queries {
		byQuery[q.Query] = q.RelevantSources[0]
	}
	r := RetrieverFunc(func(_ context.Context, query string, k int) ([]string, error) {
		out := make([]string, 0, k)
		for i := 1; i <= k; i++ {
			if i == 2 {
				out = append(out, byQuery[query])
			} else {
				out = append(out, "mem://recipes/irrelevant")
			}
		}
		return out, nil
	})

	report, err := Evaluate(context.Background(), ds, r, 1, 3, 5)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if report.NumQueries != 5 {
		t.Errorf("NumQueries = %d, want 5", report.NumQueries)
	}
	// Target at rank 2 for every query: Recall@1 = 0, Recall@3 = 1, MRR = 0.5.
	if !almostEqual(report.RecallAtK[1], 0) {
		t.Errorf("Recall@1 = %v, want 0", report.RecallAtK[1])
	}
	if !almostEqual(report.RecallAtK[3], 1) {
		t.Errorf("Recall@3 = %v, want 1", report.RecallAtK[3])
	}
	if !almostEqual(report.MRR, 0.5) {
		t.Errorf("MRR = %v, want 0.5", report.MRR)
	}
	// nDCG@3 with the single target at rank 2 = 1/log2(3) for every query.
	wantNDCG := 1 / 1.584962500721156
	if !almostEqual(report.NDCGAtK[3], wantNDCG) {
		t.Errorf("nDCG@3 = %v, want %v", report.NDCGAtK[3], wantNDCG)
	}
}

func TestFromSearchResults(t *testing.T) {
	mustURL := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return u
	}

	search := func(_ context.Context, _ string, _ int) ([]*index.SearchResult, error) {
		return []*index.SearchResult{
			{Source: mustURL("mem://a"), Score: 0.9},
			nil,                       // skipped
			{Source: nil, Score: 0.5}, // skipped
			{Source: mustURL("mem://b"), Score: 0.4},
		}, nil
	}

	ids, err := FromSearchResults(search).Retrieve(context.Background(), "q", 5)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(ids) != 2 || ids[0] != "mem://a" || ids[1] != "mem://b" {
		t.Errorf("unexpected ids: %v", ids)
	}
}

func TestEvaluateByLang(t *testing.T) {
	// Two languages, one query each: fr target at rank 1, en target at rank 2.
	ds := &Dataset{
		Name: "multi",
		Queries: []Query{
			{ID: "fr1", Query: "capitale", Lang: "fr", RelevantSources: []string{"fr-doc"}},
			{ID: "en1", Query: "capital", Lang: "en", RelevantSources: []string{"en-doc"}},
		},
	}
	r := RetrieverFunc(func(_ context.Context, query string, _ int) ([]string, error) {
		switch query {
		case "capitale":
			return []string{"fr-doc", "x"}, nil
		default:
			return []string{"x", "en-doc"}, nil
		}
	})

	report, err := Evaluate(context.Background(), ds, r, 1, 3)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	segments := report.ByLang()
	if len(segments) != 2 {
		t.Fatalf("expected 2 language segments, got %d", len(segments))
	}
	if fr := segments["fr"]; fr == nil || !almostEqual(fr.MRR, 1) {
		t.Errorf("fr segment MRR = %v, want 1", segmentMRR(fr))
	}
	if en := segments["en"]; en == nil || !almostEqual(en.MRR, 0.5) {
		t.Errorf("en segment MRR = %v, want 0.5", segmentMRR(en))
	}
	// Global MRR is the mean of the two: (1 + 0.5)/2 = 0.75.
	if !almostEqual(report.MRR, 0.75) {
		t.Errorf("global MRR = %v, want 0.75", report.MRR)
	}
}

func segmentMRR(r *Report) float64 {
	if r == nil {
		return -1
	}
	return r.MRR
}

func TestReportString(t *testing.T) {
	report := &Report{
		Dataset:    "d",
		NumQueries: 2,
		Ks:         []int{1, 5},
		MRR:        0.75,
		RecallAtK:  map[int]float64{1: 0.5, 5: 1},
		NDCGAtK:    map[int]float64{1: 0.5, 5: 0.8},
	}
	s := report.String()
	if s == "" {
		t.Fatal("empty report string")
	}
}
