package eval

import (
	"context"
	"math"
	"testing"
)

const epsilon = 1e-9

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < epsilon
}

// TestRecallAtK mirrors the worked example from the MRR tutorial: three recipe
// queries with a single relevant target each, at different ranks.
func TestRecallAtK(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		relevant  []string
		k         int
		want      float64
	}{
		// Query 1: target at rank 1 → success at every k.
		{"rank1@1", []string{"42", "18", "91", "3", "55"}, []string{"42"}, 1, 1},
		{"rank1@5", []string{"42", "18", "91", "3", "55"}, []string{"42"}, 5, 1},
		// Query 2: target at rank 3 → fail@1, success@3/@5.
		{"rank3@1", []string{"88", "33", "127", "204", "5"}, []string{"127"}, 1, 0},
		{"rank3@3", []string{"88", "33", "127", "204", "5"}, []string{"127"}, 3, 1},
		{"rank3@5", []string{"88", "33", "127", "204", "5"}, []string{"127"}, 5, 1},
		// Query 3: target at rank 23 (outside the top-5) → all fail.
		{"rank23@5", []string{"12", "45", "78", "99", "134"}, []string{"201"}, 5, 0},
		// Multiple relevant docs: set-based recall.
		{"multi@3", []string{"a", "b", "c", "d"}, []string{"a", "c", "z"}, 3, 2.0 / 3.0},
		// k larger than the result list is clamped.
		{"kbeyond", []string{"a", "b"}, []string{"b"}, 10, 1},
		// No relevant documents → 0.
		{"norelevant", []string{"a"}, nil, 5, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RecallAtK(tc.retrieved, tc.relevant, tc.k)
			if !almostEqual(got, tc.want) {
				t.Errorf("RecallAtK = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFirstRelevantRankAndReciprocal(t *testing.T) {
	cases := []struct {
		name      string
		retrieved []string
		relevant  []string
		wantRank  int
		wantRR    float64
	}{
		{"rank1", []string{"a", "b"}, []string{"a"}, 1, 1},
		{"rank2", []string{"a", "b"}, []string{"b"}, 2, 0.5},
		{"rank3", []string{"a", "b", "c"}, []string{"c"}, 3, 1.0 / 3.0},
		{"notfound", []string{"a", "b"}, []string{"z"}, -1, 0},
		{"norelevant", []string{"a"}, nil, -1, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstRelevantRank(tc.retrieved, tc.relevant); got != tc.wantRank {
				t.Errorf("FirstRelevantRank = %d, want %d", got, tc.wantRank)
			}
			if got := ReciprocalRank(tc.retrieved, tc.relevant); !almostEqual(got, tc.wantRR) {
				t.Errorf("ReciprocalRank = %v, want %v", got, tc.wantRR)
			}
		})
	}
}

// TestMRRWorkedExample reproduces the tutorial's 6-query MRR = 0.355 computation.
func TestMRRWorkedExample(t *testing.T) {
	// Reciprocal ranks: 1, 1/2, 1/3, 1/5, 1/10, 0.
	dataset := &Dataset{
		Name: "worked",
		Queries: []Query{
			{ID: "q1", Query: "q1", RelevantSources: []string{"t"}},
			{ID: "q2", Query: "q2", RelevantSources: []string{"t"}},
			{ID: "q3", Query: "q3", RelevantSources: []string{"t"}},
			{ID: "q4", Query: "q4", RelevantSources: []string{"t"}},
			{ID: "q5", Query: "q5", RelevantSources: []string{"t"}},
			{ID: "q6", Query: "q6", RelevantSources: []string{"t"}},
		},
	}

	// Build a retriever placing the target at the tutorial's ranks per query.
	ranks := map[string]int{"q1": 1, "q2": 2, "q3": 3, "q4": 5, "q5": 10, "q6": 0}
	r := RetrieverFunc(func(_ context.Context, query string, k int) ([]string, error) {
		return placeTarget(ranks[query], k), nil
	})

	report, err := Evaluate(context.Background(), dataset, r, 10)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	want := (1.0 + 0.5 + 1.0/3.0 + 0.2 + 0.1 + 0.0) / 6.0 // 0.3555...
	if !almostEqual(report.MRR, want) {
		t.Errorf("MRR = %v, want %v", report.MRR, want)
	}
}

func TestNDCGAtK(t *testing.T) {
	// Single relevant doc at rank r: nDCG@k (k>=r) = 1/log2(r+1), IDCG = 1.
	cases := []struct {
		name      string
		retrieved []string
		relevant  []string
		k         int
		want      float64
	}{
		{"rank1", []string{"a", "b", "c"}, []string{"a"}, 3, 1},
		{"rank2", []string{"a", "b", "c"}, []string{"b"}, 3, 1 / math.Log2(3)},
		{"rank3", []string{"a", "b", "c"}, []string{"c"}, 3, 1 / math.Log2(4)},
		{"outsidek", []string{"a", "b", "c"}, []string{"c"}, 2, 0},
		{"norelevant", []string{"a"}, nil, 5, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NDCGAtK(tc.retrieved, tc.relevant, tc.k)
			if !almostEqual(got, tc.want) {
				t.Errorf("NDCGAtK = %v, want %v", got, tc.want)
			}
		})
	}

	// Perfect ordering (all relevant at the top) yields nDCG = 1.
	if got := NDCGAtK([]string{"a", "b", "c"}, []string{"a", "b"}, 3); !almostEqual(got, 1) {
		t.Errorf("perfect ordering nDCG = %v, want 1", got)
	}
}

// placeTarget returns a k-long result list with the relevant "t" at rank r
// (1-indexed); r==0 means the target is absent.
func placeTarget(r, k int) []string {
	out := make([]string, 0, k)
	for i := 1; i <= k; i++ {
		if i == r {
			out = append(out, "t")
		} else {
			out = append(out, "x")
		}
	}
	return out
}
