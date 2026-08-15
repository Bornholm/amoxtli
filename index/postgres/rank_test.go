package postgres

import (
	"strings"
	"testing"
)

// TestTsvectorExprMatchesBetweenIndexingAndQuerying pins the invariant the whole
// option rests on: insertChunk, restoreRecords and ftsSearch all build their
// tsvector from this one function, so a query can never look for lexemes the
// index did not store.
func TestTsvectorExprMatchesBetweenIndexingAndQuerying(t *testing.T) {
	for _, union := range []bool{true, false} {
		i := &Index{simpleUnion: union}

		indexing := i.tsvectorExpr("$5::text::regconfig", "$4")
		querying := i.tsvectorExpr("$1::regconfig", "$2")

		if strings.Count(indexing, "to_tsvector") != strings.Count(querying, "to_tsvector") {
			t.Errorf("simpleUnion=%v: indexing builds %q, querying %q — the two must have the same shape",
				union, indexing, querying)
		}
	}
}

func TestTsvectorExprUnionIsOptional(t *testing.T) {
	with := (&Index{simpleUnion: true}).tsvectorExpr("$1::regconfig", "$2")
	if !strings.Contains(with, "'simple'") {
		t.Errorf("with the union on, expression = %q, want it to include the simple config", with)
	}

	// Without the union, only the detected language's lexemes are stored: a word
	// whose stem differs from its surface form no longer counts twice.
	without := (&Index{simpleUnion: false}).tsvectorExpr("$1::regconfig", "$2")
	if strings.Contains(without, "'simple'") {
		t.Errorf("with the union off, expression = %q, want the simple config gone", without)
	}
	if !strings.Contains(without, "unaccent") {
		t.Errorf("expression = %q, want unaccent kept in both modes", without)
	}
}

// The default must emit byte-identical SQL to what the index always ran, so
// enabling the knob is opt-in and an existing deployment sees no change.
func TestRankExprDefaultsToPlainTsRank(t *testing.T) {
	i := &Index{rankNormalization: RankNormalizationNone}

	if got, want := i.rankExpr("c.tsv", "q.query"), "ts_rank(c.tsv, q.query)"; got != want {
		t.Errorf("rankExpr() = %q, want %q", got, want)
	}
}

func TestRankExprEmitsNormalizationFlags(t *testing.T) {
	for _, tc := range []struct {
		flags int
		want  string
	}{
		{RankNormalizationLogLength, "ts_rank(c.tsv, q.query, 1)"},
		{RankNormalizationLength, "ts_rank(c.tsv, q.query, 2)"},
		{RankNormalizationScale, "ts_rank(c.tsv, q.query, 32)"},
		// The flags are a bit mask: length correction and scaling combine.
		{RankNormalizationLogLength | RankNormalizationScale, "ts_rank(c.tsv, q.query, 33)"},
	} {
		i := &Index{rankNormalization: tc.flags}
		if got := i.rankExpr("c.tsv", "q.query"); got != tc.want {
			t.Errorf("rankExpr() with flags %d = %q, want %q", tc.flags, got, tc.want)
		}
	}
}

func TestRankAndUnionOptionsRejectNonsense(t *testing.T) {
	opts := NewOptions(WithRankNormalization(-1))
	if opts.RankNormalization != RankNormalizationNone {
		t.Errorf("RankNormalization = %d, want the default %d", opts.RankNormalization, RankNormalizationNone)
	}

	// The union defaults to on: every index built so far stores both.
	if !NewOptions().SimpleConfigUnion {
		t.Error("SimpleConfigUnion defaults to false, want true to match existing indexes")
	}
}
