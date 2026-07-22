package filternorm

import (
	"reflect"
	"testing"
	"time"
)

func TestValueCanonicalizesDates(t *testing.T) {
	instant := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	canonical := "2026-07-13T12:00:00.000000000Z"

	testCases := []struct {
		name string
		in   any
		want any
	}{
		{"time.Time", instant, canonical},
		{"time.Time in another zone", instant.In(time.FixedZone("CEST", 2*3600)), canonical},
		{"RFC 3339 string", "2026-07-13T12:00:00Z", canonical},
		{"RFC 3339 string with offset", "2026-07-13T14:00:00+02:00", canonical},
		{"already canonical", canonical, canonical},
		{"plain string", "william", "william"},
		{"date-looking but not RFC 3339", "2026-07-13", "2026-07-13"},
		{"number", 3.0, 3.0},
		{"bool", true, true},
		{"nil", nil, nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := Value(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Value(%#v) = %#v, want %#v", tc.in, got, tc.want)
			}

			// Applying the canonicalization to already-stored metadata must be
			// a no-op, otherwise a re-ingested document would drift.
			if again := Value(got); !reflect.DeepEqual(again, got) {
				t.Fatalf("Value is not idempotent: %#v then %#v", got, again)
			}
		})
	}
}

// Canonical dates must sort lexicographically the way they sort
// chronologically: this is the property the SQL push-down of ordered operators
// will rely on.
func TestCanonicalOrderMatchesChronologicalOrder(t *testing.T) {
	instants := []time.Time{
		time.Date(2024, 1, 2, 9, 0, 0, 0, time.UTC),
		time.Date(2024, 1, 2, 10, 0, 0, 0, time.FixedZone("CEST", 2*3600)), // 08:00Z, earlier
		time.Date(2024, 1, 2, 9, 0, 0, 1, time.UTC),
		time.Date(2023, 12, 31, 23, 59, 59, 999999999, time.UTC),
	}

	for _, a := range instants {
		for _, b := range instants {
			want := a.Compare(b)

			got, ok := Compare(FormatTime(a), FormatTime(b))
			if !ok {
				t.Fatalf("Compare(%s, %s) is not comparable", a, b)
			}

			if got != want {
				t.Fatalf("Compare(%s, %s) = %d, want %d", a, b, got, want)
			}
		}
	}
}

func TestMetadataCopiesAndCanonicalizes(t *testing.T) {
	source := map[string]any{
		"author": "william",
		"date":   time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		"tags":   []any{"go", "db"},
	}

	normalized := Metadata(source)

	if _, isTime := source["date"].(time.Time); !isTime {
		t.Fatal("Metadata mutated its input")
	}

	if got := normalized["date"]; got != "2026-07-13T12:00:00.000000000Z" {
		t.Fatalf("normalized date = %#v", got)
	}

	if !reflect.DeepEqual(normalized["tags"], source["tags"]) {
		t.Fatalf("containers must be carried over untouched, got %#v", normalized["tags"])
	}

	if Metadata(nil) != nil {
		t.Fatal("Metadata(nil) should stay nil so no empty JSON blob is persisted")
	}
}
