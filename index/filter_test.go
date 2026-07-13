package index

import (
	"testing"
	"time"
)

func TestFilterMatches(t *testing.T) {
	meta := map[string]any{
		"author": "william",
		"lang":   "fr",
		"year":   float64(2026), // JSON-decoded number
		"count":  3,             // native int
		"public": true,
		"date":   "2026-07-13T00:00:00Z",
	}

	testCases := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"empty matches all", Filter{}, true},
		{"eq string", Filter{Eq("author", "william")}, true},
		{"eq string mismatch", Filter{Eq("author", "bob")}, false},
		{"ne string", Filter{Ne("author", "bob")}, true},
		{"ne on absent key", Filter{Ne("missing", "x")}, true},
		{"eq on absent key", Filter{Eq("missing", "x")}, false},
		{"conjunction ok", Filter{Eq("author", "william"), Eq("lang", "fr")}, true},
		{"conjunction one fails", Filter{Eq("author", "william"), Eq("lang", "en")}, false},
		{"eq number int vs float", Filter{Eq("year", 2026)}, true},
		{"eq number float vs int", Filter{Eq("count", float64(3))}, true},
		{"gt number", Filter{Gt("year", 2000)}, true},
		{"gt number false", Filter{Gt("year", 2030)}, false},
		{"gte number boundary", Filter{Gte("year", 2026)}, true},
		{"lt number", Filter{Lt("count", 5)}, true},
		{"lte boundary", Filter{Lte("count", 3)}, true},
		{"in match", Filter{In("lang", "en", "fr", "de")}, true},
		{"in no match", Filter{In("lang", "en", "de")}, false},
		{"in numbers", Filter{In("count", 1, 2, 3)}, true},
		{"bool eq", Filter{Eq("public", true)}, true},
		{"bool ne", Filter{Ne("public", false)}, true},
		{"ordered on non-comparable types", Filter{Gt("author", 3)}, false},
		{"date gte string", Filter{Gte("date", "2026-01-01T00:00:00Z")}, true},
		{"date lt string false", Filter{Lt("date", "2026-01-01T00:00:00Z")}, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.filter.Matches(meta); got != tc.want {
				t.Fatalf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFilterDateAgainstTimeValue(t *testing.T) {
	// A time.Time condition value must compare against an RFC 3339 string meta.
	meta := map[string]any{"date": "2026-07-13T12:00:00Z"}
	cutoff := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)

	if !(Filter{Gt("date", cutoff)}).Matches(meta) {
		t.Fatal("expected date after cutoff to match Gt")
	}
	if (Filter{Lt("date", cutoff)}).Matches(meta) {
		t.Fatal("expected date after cutoff to not match Lt")
	}
}

func TestFilterNilMeta(t *testing.T) {
	if !(Filter{Ne("k", "v")}).Matches(nil) {
		t.Fatal("Ne on nil meta should match")
	}
	if (Filter{Eq("k", "v")}).Matches(nil) {
		t.Fatal("Eq on nil meta should not match")
	}
	if !(Filter{}).Matches(nil) {
		t.Fatal("empty filter on nil meta should match")
	}
}
