// Package filtertest is the shared conformance suite for index.Filter
// semantics.
//
// Every implementation of those semantics — the Go evaluator, and each backend
// that pushes the filter down into its search query — must pass the exact same
// cases. This is the differential test protecting the invariant
// matchGo(f, metadata) == matchSQL(f, metadata): a backend whose translation
// disagrees on absent keys, on int-versus-float, or on a date offset would
// otherwise return different documents for the same query, with nothing failing
// loudly.
//
// A backend advertising filter push-down is expected to run Run against a real
// index before advertising it.
package filtertest

import (
	"context"
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/filternorm"
)

// Case is one document's metadata, one filter, and the expected verdict.
type Case struct {
	Name     string
	Filter   index.Filter
	Metadata map[string]any
	Want     bool
}

// Evaluator applies a filter to a document carrying the given metadata and
// reports whether the document is kept. A SQL implementation typically inserts
// the document, runs the filtered query and checks whether it comes back.
//
// The metadata handed over is already canonicalized the way the write path
// canonicalizes it (see filternorm.Metadata), so an implementation may store it
// verbatim.
type Evaluator func(ctx context.Context, filter index.Filter, metadata map[string]any) (bool, error)

// Run executes every case of the suite against eval.
func Run(t *testing.T, eval Evaluator) {
	t.Helper()

	ctx := t.Context()

	for _, c := range Cases {
		t.Run(c.Name, func(t *testing.T) {
			got, err := eval(ctx, c.Filter, filternorm.Metadata(c.Metadata))
			if err != nil {
				t.Fatalf("evaluating filter %#v: %+v", c.Filter, err)
			}

			if got != c.Want {
				t.Errorf("filter %#v against %#v = %v, want %v", c.Filter, c.Metadata, got, c.Want)
			}
		})
	}
}

var (
	refTime   = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	refString = filternorm.FormatTime(refTime)
	// Same instant, expressed in another zone with another precision: it must
	// compare equal once canonicalized.
	refOffset = "2026-07-13T14:00:00+02:00"
)

// Cases covers each operator against each value kind, plus the edge cases where
// implementations are most likely to drift apart.
var Cases = []Case{
	// --- Empty filter -------------------------------------------------------
	{"empty-filter-matches", index.Filter{}, map[string]any{"a": "x"}, true},
	{"empty-filter-matches-empty-metadata", index.Filter{}, map[string]any{}, true},
	{"empty-filter-matches-nil-metadata", index.Filter{}, nil, true},

	// --- Eq -----------------------------------------------------------------
	{"eq-string", index.Filter{index.Eq("a", "x")}, map[string]any{"a": "x"}, true},
	{"eq-string-mismatch", index.Filter{index.Eq("a", "x")}, map[string]any{"a": "y"}, false},
	{"eq-string-case-sensitive", index.Filter{index.Eq("a", "X")}, map[string]any{"a": "x"}, false},
	{"eq-string-accent-sensitive", index.Filter{index.Eq("a", "eric")}, map[string]any{"a": "éric"}, false},
	{"eq-string-empty", index.Filter{index.Eq("a", "")}, map[string]any{"a": ""}, true},
	{"eq-missing-key", index.Filter{index.Eq("a", "x")}, map[string]any{}, false},
	{"eq-missing-key-nil-metadata", index.Filter{index.Eq("a", "x")}, nil, false},
	{"eq-other-key-present", index.Filter{index.Eq("a", "x")}, map[string]any{"b": "x"}, false},
	{"eq-int-vs-float", index.Filter{index.Eq("n", 3)}, map[string]any{"n": 3.0}, true},
	{"eq-float-vs-int", index.Filter{index.Eq("n", 3.0)}, map[string]any{"n": 3}, true},
	{"eq-int64-vs-float", index.Filter{index.Eq("n", int64(3))}, map[string]any{"n": 3.0}, true},
	{"eq-number-mismatch", index.Filter{index.Eq("n", 3)}, map[string]any{"n": 4.0}, false},
	{"eq-negative-number", index.Filter{index.Eq("n", -1.5)}, map[string]any{"n": -1.5}, true},
	{"eq-zero-number", index.Filter{index.Eq("n", 0)}, map[string]any{"n": 0.0}, true},
	{"eq-number-vs-string", index.Filter{index.Eq("n", 3)}, map[string]any{"n": "3"}, false},
	{"eq-string-vs-number", index.Filter{index.Eq("n", "3")}, map[string]any{"n": 3.0}, false},
	{"eq-bool-true", index.Filter{index.Eq("b", true)}, map[string]any{"b": true}, true},
	{"eq-bool-false", index.Filter{index.Eq("b", false)}, map[string]any{"b": false}, true},
	{"eq-bool-mismatch", index.Filter{index.Eq("b", true)}, map[string]any{"b": false}, false},
	{"eq-bool-vs-string", index.Filter{index.Eq("b", true)}, map[string]any{"b": "true"}, false},
	{"eq-bool-vs-number", index.Filter{index.Eq("b", true)}, map[string]any{"b": 1.0}, false},
	{"eq-null-value", index.Filter{index.Eq("a", "x")}, map[string]any{"a": nil}, false},

	// --- Eq on dates --------------------------------------------------------
	{"eq-time-vs-canonical-string", index.Filter{index.Eq("d", refTime)}, map[string]any{"d": refString}, true},
	{"eq-time-vs-time", index.Filter{index.Eq("d", refTime)}, map[string]any{"d": refTime}, true},
	{"eq-time-vs-other-offset", index.Filter{index.Eq("d", refTime)}, map[string]any{"d": refOffset}, true},
	{"eq-rfc3339-operand-vs-time", index.Filter{index.Eq("d", "2026-07-13T12:00:00Z")}, map[string]any{"d": refTime}, true},
	{"eq-time-mismatch", index.Filter{index.Eq("d", refTime.Add(time.Second))}, map[string]any{"d": refTime}, false},

	// --- Ne (the absent-key trap) -------------------------------------------
	{"ne-string", index.Filter{index.Ne("a", "x")}, map[string]any{"a": "y"}, true},
	{"ne-string-equal", index.Filter{index.Ne("a", "x")}, map[string]any{"a": "x"}, false},
	{"ne-missing-key", index.Filter{index.Ne("a", "x")}, map[string]any{}, false},
	{"ne-missing-key-nil-metadata", index.Filter{index.Ne("a", "x")}, nil, false},
	{"ne-null-value", index.Filter{index.Ne("a", "x")}, map[string]any{"a": nil}, true},
	{"ne-cross-type", index.Filter{index.Ne("n", 3)}, map[string]any{"n": "abc"}, true},
	{"ne-int-vs-float", index.Filter{index.Ne("n", 3)}, map[string]any{"n": 3.0}, false},
	{"ne-bool", index.Filter{index.Ne("b", false)}, map[string]any{"b": true}, true},

	// --- In -----------------------------------------------------------------
	{"in-string-match", index.Filter{index.In("a", "x", "y")}, map[string]any{"a": "y"}, true},
	{"in-string-no-match", index.Filter{index.In("a", "x", "y")}, map[string]any{"a": "z"}, false},
	{"in-empty", index.Filter{index.In("a")}, map[string]any{"a": "x"}, false},
	{"in-missing-key", index.Filter{index.In("a", "x")}, map[string]any{}, false},
	{"in-numbers-int-vs-float", index.Filter{index.In("n", 1, 2, 3)}, map[string]any{"n": 3.0}, true},
	{"in-numbers-no-match", index.Filter{index.In("n", 1, 2)}, map[string]any{"n": 3.0}, false},
	{"in-mixed-types", index.Filter{index.In("a", 1, "x", true)}, map[string]any{"a": "x"}, true},
	{"in-cross-type-only", index.Filter{index.In("n", "3")}, map[string]any{"n": 3.0}, false},
	{"in-bool", index.Filter{index.In("b", true)}, map[string]any{"b": true}, true},

	// --- Ordered operators, numbers -----------------------------------------
	{"gt-number", index.Filter{index.Gt("n", 2)}, map[string]any{"n": 3.0}, true},
	{"gt-number-equal", index.Filter{index.Gt("n", 3)}, map[string]any{"n": 3.0}, false},
	{"gt-number-below", index.Filter{index.Gt("n", 4)}, map[string]any{"n": 3.0}, false},
	{"gte-number-equal", index.Filter{index.Gte("n", 3)}, map[string]any{"n": 3.0}, true},
	{"lt-number", index.Filter{index.Lt("n", 4)}, map[string]any{"n": 3.0}, true},
	{"lt-number-equal", index.Filter{index.Lt("n", 3)}, map[string]any{"n": 3.0}, false},
	{"lte-number-equal", index.Filter{index.Lte("n", 3)}, map[string]any{"n": 3.0}, true},
	{"gt-negative-number", index.Filter{index.Gt("n", -2)}, map[string]any{"n": -1.0}, true},
	{"gt-missing-key", index.Filter{index.Gt("n", 2)}, map[string]any{}, false},
	{"gt-null-value", index.Filter{index.Gt("n", 2)}, map[string]any{"n": nil}, false},

	// --- Ordered operators, cross-type (total, never an error) --------------
	{"gt-cross-type-string-operand", index.Filter{index.Gt("n", "abc")}, map[string]any{"n": 3.0}, false},
	{"gt-cross-type-number-operand", index.Filter{index.Gt("a", 3)}, map[string]any{"a": "abc"}, false},
	{"lt-cross-type", index.Filter{index.Lt("a", 3)}, map[string]any{"a": "abc"}, false},
	{"gt-bool-metadata", index.Filter{index.Gt("b", true)}, map[string]any{"b": true}, false},
	{"gte-bool-metadata", index.Filter{index.Gte("b", true)}, map[string]any{"b": true}, false},

	// --- Ordered operators, strings -----------------------------------------
	{"gt-string", index.Filter{index.Gt("a", "abc")}, map[string]any{"a": "abd"}, true},
	{"lt-string", index.Filter{index.Lt("a", "abd")}, map[string]any{"a": "abc"}, true},
	{"gte-string-equal", index.Filter{index.Gte("a", "abc")}, map[string]any{"a": "abc"}, true},

	// --- Ordered operators, dates -------------------------------------------
	{"gt-time-operand", index.Filter{index.Gt("d", refTime.Add(-time.Hour))}, map[string]any{"d": refString}, true},
	{"gt-time-operand-after", index.Filter{index.Gt("d", refTime.Add(time.Hour))}, map[string]any{"d": refString}, false},
	{"gte-time-operand-equal", index.Filter{index.Gte("d", refTime)}, map[string]any{"d": refString}, true},
	{"lt-time-operand", index.Filter{index.Lt("d", refTime.Add(time.Hour))}, map[string]any{"d": refString}, true},
	{"lte-time-operand-equal", index.Filter{index.Lte("d", refTime)}, map[string]any{"d": refString}, true},
	{"gt-rfc3339-operand", index.Filter{index.Gt("d", "2026-01-01T00:00:00Z")}, map[string]any{"d": refString}, true},
	// The zone-crossing case that lexicographic comparison gets wrong without
	// canonicalization: 14:00+02:00 is 12:00Z, so it is NOT after 13:00Z.
	{"gt-offset-metadata-canonicalized", index.Filter{index.Gt("d", "2026-07-13T13:00:00Z")}, map[string]any{"d": refOffset}, false},
	{"lt-offset-metadata-canonicalized", index.Filter{index.Lt("d", "2026-07-13T13:00:00Z")}, map[string]any{"d": refOffset}, true},
	{"gt-time-metadata-value", index.Filter{index.Gt("d", refTime.Add(-time.Hour))}, map[string]any{"d": refTime}, true},

	// --- Exists / NotExists -------------------------------------------------
	{"exists-present", index.Filter{index.Exists("a")}, map[string]any{"a": "x"}, true},
	{"exists-present-null-value", index.Filter{index.Exists("a")}, map[string]any{"a": nil}, true},
	{"exists-missing", index.Filter{index.Exists("a")}, map[string]any{}, false},
	{"exists-nil-metadata", index.Filter{index.Exists("a")}, nil, false},
	{"not-exists-missing", index.Filter{index.NotExists("a")}, map[string]any{}, true},
	{"not-exists-nil-metadata", index.Filter{index.NotExists("a")}, nil, true},
	{"not-exists-present", index.Filter{index.NotExists("a")}, map[string]any{"a": "x"}, false},
	{"not-exists-present-null-value", index.Filter{index.NotExists("a")}, map[string]any{"a": nil}, false},

	// --- Containers: present, never comparable ------------------------------
	{"eq-array-metadata", index.Filter{index.Eq("tags", "go")}, map[string]any{"tags": []any{"go", "db"}}, false},
	{"ne-array-metadata", index.Filter{index.Ne("tags", "go")}, map[string]any{"tags": []any{"go", "db"}}, true},
	{"exists-array-metadata", index.Filter{index.Exists("tags")}, map[string]any{"tags": []any{"go"}}, true},
	{"gt-array-metadata", index.Filter{index.Gt("tags", "go")}, map[string]any{"tags": []any{"go"}}, false},
	{"in-array-metadata", index.Filter{index.In("tags", "go")}, map[string]any{"tags": []any{"go"}}, false},
	{"eq-object-metadata", index.Filter{index.Eq("o", "x")}, map[string]any{"o": map[string]any{"k": "x"}}, false},

	// --- Conjunction --------------------------------------------------------
	{"and-all-true", index.Filter{index.Eq("a", "x"), index.Gt("n", 2)}, map[string]any{"a": "x", "n": 3.0}, true},
	{"and-one-false", index.Filter{index.Eq("a", "x"), index.Gt("n", 4)}, map[string]any{"a": "x", "n": 3.0}, false},
	{"and-one-missing-key", index.Filter{index.Eq("a", "x"), index.Eq("b", "y")}, map[string]any{"a": "x"}, false},
	{"and-exists-and-ne", index.Filter{index.Exists("a"), index.Ne("a", "x")}, map[string]any{"a": "y"}, true},

	// --- Hostile keys and values (no interpolation, no error) ---------------
	{"hostile-key-treated-as-literal", index.Filter{index.Eq("'; DROP TABLE documents; --", "x")}, map[string]any{"a": "x"}, false},
	{"hostile-value-treated-as-literal", index.Filter{index.Eq("a", "'; DROP TABLE documents; --")}, map[string]any{"a": "x"}, false},
	{"hostile-value-matches-itself", index.Filter{index.Eq("a", "' OR 1=1 --")}, map[string]any{"a": "' OR 1=1 --"}, true},
	{"json-path-value", index.Filter{index.Eq("a", `$."a"[0]`)}, map[string]any{"a": `$."a"[0]`}, true},
	{"quote-in-value", index.Filter{index.Eq("a", `he said "hi"`)}, map[string]any{"a": `he said "hi"`}, true},
	{"unicode-value", index.Filter{index.Eq("a", "日本語")}, map[string]any{"a": "日本語"}, true},
}
