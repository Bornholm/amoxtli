package postgres

import "testing"

// TestEFSearchCoversRequestedLimit pins the property the whole setting exists
// for: the HNSW result heap is never smaller than the number of rows the query
// asks for. pgvector's default of 40 would silently cap the vector leg on the
// large candidate windows the search pipeline uses (up to 500).
func TestEFSearchCoversRequestedLimit(t *testing.T) {
	i := &Index{efSearchFactor: DefaultEFSearchFactor}

	for _, limit := range []int{1, 10, 40, 100, 500, 900} {
		if got := i.efSearch(limit); got < limit {
			t.Errorf("efSearch(%d) = %d, want at least %d", limit, got, limit)
		}
	}
}

func TestEFSearchStaysWithinPgvectorBounds(t *testing.T) {
	i := &Index{efSearchFactor: DefaultEFSearchFactor}

	// Never below the server default: raising recall must not become lowering it.
	if got := i.efSearch(1); got != minEFSearch {
		t.Errorf("efSearch(1) = %d, want the pgvector default %d", got, minEFSearch)
	}

	// pgvector rejects an ef_search above 1000, so an oversized limit must be
	// clamped rather than turned into a failing query.
	if got := i.efSearch(100_000); got != maxEFSearch {
		t.Errorf("efSearch(100000) = %d, want it clamped to %d", got, maxEFSearch)
	}
}

func TestEFSearchHonorsFactor(t *testing.T) {
	dense := &Index{efSearchFactor: 4}
	if got, want := dense.efSearch(100), 400; got != want {
		t.Errorf("efSearch(100) with factor 4 = %d, want %d", got, want)
	}

	// Factor 1 is the strict minimum: exactly the requested limit, no over-scan.
	tight := &Index{efSearchFactor: 1}
	if got, want := tight.efSearch(200), 200; got != want {
		t.Errorf("efSearch(200) with factor 1 = %d, want %d", got, want)
	}
}

func TestWithEFSearchFactorRejectsNonsense(t *testing.T) {
	opts := NewOptions(WithEFSearchFactor(0), WithEFSearchFactor(-3))

	// A zero or negative factor would produce a heap of zero rows; the option
	// keeps the default instead.
	if opts.EFSearchFactor != DefaultEFSearchFactor {
		t.Errorf("EFSearchFactor = %d, want the default %d", opts.EFSearchFactor, DefaultEFSearchFactor)
	}
}
