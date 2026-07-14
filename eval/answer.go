package eval

import (
	"strings"
	"unicode"
)

// This file holds the answer-quality metrics used by the optional generation
// (reader) evaluation: given a generated answer and a set of acceptable gold
// answers, score it with the standard SQuAD-style Exact Match and token-overlap
// F1. Like the ranking metrics, these functions are pure and dependency-free so
// they run in the short unit suite; the reader that produces the answers lives in
// the gated integration harness.

// NormalizeAnswer applies the canonical SQuAD answer normalisation before
// comparison: lower-case, drop punctuation, drop the articles a/an/the, and
// collapse runs of whitespace. This makes "The Eiffel Tower." and "eiffel
// tower" compare equal, so EM/F1 measure answer content rather than surface
// formatting.
func NormalizeAnswer(s string) string {
	s = strings.ToLower(s)

	// Drop punctuation (replace with space so "u.s.a" -> "u s a" -> tokens).
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}

	// Drop articles and collapse whitespace.
	fields := strings.Fields(b.String())
	out := fields[:0]
	for _, f := range fields {
		if f == "a" || f == "an" || f == "the" {
			continue
		}
		out = append(out, f)
	}
	return strings.Join(out, " ")
}

// answerTokens returns the normalised whitespace tokens of an answer.
func answerTokens(s string) []string {
	n := NormalizeAnswer(s)
	if n == "" {
		return nil
	}
	return strings.Split(n, " ")
}

// AnswerExactMatch is 1.0 when the prediction, after normalisation, equals any
// of the gold answers, else 0.0. With no gold answers it returns 0.
func AnswerExactMatch(pred string, golds []string) float64 {
	np := NormalizeAnswer(pred)
	for _, g := range golds {
		if np == NormalizeAnswer(g) {
			return 1.0
		}
	}
	return 0.0
}

// AnswerF1 is the maximum SQuAD token-overlap F1 between the prediction and any
// gold answer: with p = |common|/|pred| and r = |common|/|gold| over the multiset
// of normalised tokens, F1 = 2pr/(p+r). It credits partial answers that a strict
// Exact Match misses. Two empty (after normalisation) strings count as a perfect
// match — matching the SQuAD convention for yes/no and no-answer cases — while an
// empty prediction against a non-empty gold (or vice-versa) scores 0.
func AnswerF1(pred string, golds []string) float64 {
	predTokens := answerTokens(pred)
	best := 0.0
	for _, g := range golds {
		goldTokens := answerTokens(g)
		if len(predTokens) == 0 || len(goldTokens) == 0 {
			// Both empty → 1; only one empty → 0 (no overlap possible).
			if len(predTokens) == 0 && len(goldTokens) == 0 {
				return 1.0
			}
			continue
		}
		common := countCommon(predTokens, goldTokens)
		if common == 0 {
			continue
		}
		p := float64(common) / float64(len(predTokens))
		r := float64(common) / float64(len(goldTokens))
		f1 := 2 * p * r / (p + r)
		if f1 > best {
			best = f1
		}
	}
	return best
}

// countCommon returns the size of the multiset intersection of a and b.
func countCommon(a, b []string) int {
	counts := make(map[string]int, len(a))
	for _, t := range a {
		counts[t]++
	}
	common := 0
	for _, t := range b {
		if counts[t] > 0 {
			counts[t]--
			common++
		}
	}
	return common
}
