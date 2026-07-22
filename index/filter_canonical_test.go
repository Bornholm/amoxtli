package index_test

import (
	"testing"
	"time"

	"github.com/bornholm/amoxtli/index"
)

// Filters that select the same documents must encode identically, whatever the
// syntax used to build them: the encoding identifies a filter by its meaning.
func TestCanonicalBytesIgnoresEquivalentSpellings(t *testing.T) {
	instant := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	testCases := []struct {
		name string
		a, b index.Filter
	}{
		{
			name: "condition order",
			a:    index.Filter{index.Eq("a", "x"), index.Gt("n", 2)},
			b:    index.Filter{index.Gt("n", 2), index.Eq("a", "x")},
		},
		{
			name: "int versus float operand",
			a:    index.Filter{index.Eq("n", 3)},
			b:    index.Filter{index.Eq("n", 3.0)},
		},
		{
			name: "int64 versus float operand",
			a:    index.Filter{index.Gte("n", int64(3))},
			b:    index.Filter{index.Gte("n", 3.0)},
		},
		{
			name: "date across timezones",
			a:    index.Filter{index.Gt("d", instant)},
			b:    index.Filter{index.Gt("d", "2026-07-13T14:00:00+02:00")},
		},
		{
			name: "In operand order",
			a:    index.Filter{index.In("t", "x", "y", "z")},
			b:    index.Filter{index.In("t", "z", "x", "y")},
		},
		{
			name: "In numeric operand types",
			a:    index.Filter{index.In("n", 1, 2)},
			b:    index.Filter{index.In("n", 2.0, 1.0)},
		},
		{
			name: "empty and nil filters",
			a:    index.Filter{},
			b:    nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := string(tc.a.CanonicalBytes()), string(tc.b.CanonicalBytes()); got != want {
				t.Fatalf("equivalent filters encoded differently:\n %q\n %q", got, want)
			}
		})
	}
}

// Filters that select differently must encode differently, including the pairs
// a naive encoding would collide.
func TestCanonicalBytesSeparatesDistinctFilters(t *testing.T) {
	testCases := []struct {
		name string
		a, b index.Filter
	}{
		{"different key", index.Filter{index.Eq("a", "x")}, index.Filter{index.Eq("b", "x")}},
		{"different value", index.Filter{index.Eq("a", "x")}, index.Filter{index.Eq("a", "y")}},
		{"different operator", index.Filter{index.Eq("a", "x")}, index.Filter{index.Ne("a", "x")}},
		{"gt versus gte", index.Filter{index.Gt("n", 2)}, index.Filter{index.Gte("n", 2)}},
		{"exists versus not exists", index.Filter{index.Exists("a")}, index.Filter{index.NotExists("a")}},
		{"extra condition", index.Filter{index.Eq("a", "x")}, index.Filter{index.Eq("a", "x"), index.Eq("b", "y")}},
		{"empty versus non-empty", index.Filter{}, index.Filter{index.Eq("a", "x")}},
		{"number versus its string form", index.Filter{index.Eq("n", 3)}, index.Filter{index.Eq("n", "3")}},
		{"bool versus its string form", index.Filter{index.Eq("b", true)}, index.Filter{index.Eq("b", "true")}},
		{"In subset", index.Filter{index.In("t", "x")}, index.Filter{index.In("t", "x", "y")}},
		// A value containing the field separators must not be able to forge the
		// encoding of a different condition.
		{"separator smuggling", index.Filter{index.Eq("a", "x\x00eq\x00sy")}, index.Filter{index.Eq("a", "x"), index.Eq("y", "z")}},
		{"newline smuggling", index.Filter{index.Eq("a", "x\"\n\"b\"\x00eq")}, index.Filter{index.Eq("a", "x"), index.Eq("b", "")}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got, other := string(tc.a.CanonicalBytes()), string(tc.b.CanonicalBytes()); got == other {
				t.Fatalf("distinct filters encoded identically: %q", got)
			}
		})
	}
}

// The encoding must not depend on map iteration order or any other run-to-run
// variation.
func TestCanonicalBytesIsStable(t *testing.T) {
	filter := index.Filter{
		index.In("t", "z", "a", "m"),
		index.Eq("n", 3),
		index.Exists("d"),
		// An operand kind the evaluator never matches still has to encode
		// deterministically.
		index.Eq("o", map[string]any{"b": 2, "a": 1}),
	}

	first := string(filter.CanonicalBytes())
	for range 100 {
		if got := string(filter.CanonicalBytes()); got != first {
			t.Fatalf("unstable encoding:\n %q\n %q", first, got)
		}
	}
}

func TestCanonicalBytesEmptyIsNil(t *testing.T) {
	if got := (index.Filter{}).CanonicalBytes(); got != nil {
		t.Fatalf("an empty filter must encode to nil, got %#v", got)
	}
}
