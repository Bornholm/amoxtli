package eval

import (
	"math"
	"testing"
)

func TestNormalizeAnswer(t *testing.T) {
	cases := map[string]string{
		"The Eiffel Tower.":   "eiffel tower",
		"  a  QUICK  brown  ": "quick brown",
		"U.S.A.":              "u s", // stray "a" is stripped as an article, per SQuAD
		"an Apple":            "apple",
		"the the the":         "",
		"yes":                 "yes",
	}
	for in, want := range cases {
		if got := NormalizeAnswer(in); got != want {
			t.Errorf("NormalizeAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAnswerExactMatch(t *testing.T) {
	golds := []string{"Paris", "the city of Paris"}
	if AnswerExactMatch("PARIS.", golds) != 1.0 {
		t.Error("normalised exact match should hit")
	}
	if AnswerExactMatch("the city of paris", golds) != 1.0 {
		t.Error("should match the second gold after normalisation")
	}
	if AnswerExactMatch("London", golds) != 0.0 {
		t.Error("wrong answer should not match")
	}
	if AnswerExactMatch("Paris", nil) != 0.0 {
		t.Error("no golds should score 0")
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestAnswerF1(t *testing.T) {
	// Perfect overlap.
	if got := AnswerF1("Barack Obama", []string{"Barack Obama"}); !approx(got, 1.0) {
		t.Errorf("identical F1 = %.4f, want 1", got)
	}
	// Partial overlap: pred 2 tokens, gold 3 tokens, 2 common.
	// p = 2/2 = 1, r = 2/3, F1 = 2*1*(2/3)/(1+2/3) = (4/3)/(5/3) = 0.8.
	if got := AnswerF1("Barack Obama", []string{"Barack Hussein Obama"}); !approx(got, 0.8) {
		t.Errorf("partial F1 = %.4f, want 0.8", got)
	}
	// No overlap.
	if got := AnswerF1("cat", []string{"dog"}); !approx(got, 0.0) {
		t.Errorf("disjoint F1 = %.4f, want 0", got)
	}
	// Best over several golds wins.
	if got := AnswerF1("New York City", []string{"Chicago", "New York City"}); !approx(got, 1.0) {
		t.Errorf("multi-gold F1 = %.4f, want 1", got)
	}
	// yes/no answers behave like single tokens.
	if got := AnswerF1("Yes", []string{"yes"}); !approx(got, 1.0) {
		t.Errorf("yes/no F1 = %.4f, want 1", got)
	}
}

func TestAnswerF1EmptyCases(t *testing.T) {
	// Article-only strings normalise to empty; two empties are a perfect match.
	if got := AnswerF1("the", []string{"a"}); !approx(got, 1.0) {
		t.Errorf("both-empty F1 = %.4f, want 1", got)
	}
	// One empty, one not → 0.
	if got := AnswerF1("the", []string{"Paris"}); !approx(got, 0.0) {
		t.Errorf("one-empty F1 = %.4f, want 0", got)
	}
}
