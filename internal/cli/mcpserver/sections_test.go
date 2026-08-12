package mcpserver

import (
	"net/url"
	"slices"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/model"
)

// sectionIDs builds n stable section IDs, named after their position so a
// failure reads as an order rather than as opaque identifiers.
func sectionIDs(names ...string) []model.SectionID {
	ids := make([]model.SectionID, 0, len(names))
	for _, name := range names {
		ids = append(ids, model.SectionID(name))
	}
	return ids
}

// TestBestSectionsKeepsHighestScoring: a search returning every matched section
// of a document is what saturates an agent's context window, so only the best
// scoring ones are handed back.
func TestBestSectionsKeepsHighestScoring(t *testing.T) {
	result := &index.SearchResult{
		Sections: sectionIDs("a", "b", "c", "d", "e"),
		SectionScores: map[model.SectionID]float64{
			"a": 0.1,
			"b": 0.9,
			"c": 0.2,
			"d": 0.8,
			"e": 0.3,
		},
	}

	kept := bestSections(result, 2)

	if want := sectionIDs("b", "d"); !slices.Equal(kept, want) {
		t.Fatalf("expected the two highest scoring sections %v, got %v", want, kept)
	}
}

// TestBestSectionsPreservesDocumentOrder: sections of one document read as a
// sequence, so the kept ones are handed back in their original order rather
// than shuffled by score.
func TestBestSectionsPreservesDocumentOrder(t *testing.T) {
	result := &index.SearchResult{
		Sections: sectionIDs("first", "second", "third"),
		SectionScores: map[model.SectionID]float64{
			"first":  0.5,
			"second": 0.1,
			"third":  0.9,
		},
	}

	kept := bestSections(result, 2)

	if want := sectionIDs("first", "third"); !slices.Equal(kept, want) {
		t.Fatalf("expected document order %v, got %v", want, kept)
	}
}

// TestBestSectionsWithoutScores: backends that do not score sections
// individually already return them in relevance order, so the leading ones are
// kept.
func TestBestSectionsWithoutScores(t *testing.T) {
	result := &index.SearchResult{Sections: sectionIDs("a", "b", "c")}

	kept := bestSections(result, 2)

	if want := sectionIDs("a", "b"); !slices.Equal(kept, want) {
		t.Fatalf("expected the leading sections %v, got %v", want, kept)
	}
}

// TestBestSectionsUnbounded: a non-positive bound, or a result already below
// it, is returned untouched.
func TestBestSectionsUnbounded(t *testing.T) {
	all := sectionIDs("a", "b", "c")
	result := &index.SearchResult{Sections: all}

	if kept := bestSections(result, 0); !slices.Equal(kept, all) {
		t.Fatalf("expected every section without a bound, got %v", kept)
	}
	if kept := bestSections(result, 10); !slices.Equal(kept, all) {
		t.Fatalf("expected every section below the bound, got %v", kept)
	}
}

// TestMaxSectionsPerResultConfig: unset means the default bound, negative means
// no bound at all — the escape hatch for a client that wants everything.
func TestMaxSectionsPerResultConfig(t *testing.T) {
	var cfg config.Config

	if got := cfg.MaxSectionsPerResult(); got != config.DefaultMCPMaxSectionsPerResult {
		t.Fatalf("expected the default bound %d, got %d", config.DefaultMCPMaxSectionsPerResult, got)
	}

	cfg.MCP.MaxSectionsPerResult = -1
	if got := cfg.MaxSectionsPerResult(); got != 0 {
		t.Fatalf("expected a negative setting to mean no bound, got %d", got)
	}

	cfg.MCP.MaxSectionsPerResult = 7
	if got := cfg.MaxSectionsPerResult(); got != 7 {
		t.Fatalf("expected the configured bound, got %d", got)
	}
}

// TestSliceContentWholeSection: without a range, a section is returned whole
// and reports no continuation.
func TestSliceContentWholeSection(t *testing.T) {
	got := sliceContent("hello", 0, 0)

	if got.Content != "hello" {
		t.Fatalf("expected the whole content, got %q", got.Content)
	}
	if got.Offset != 0 || got.Length != 5 || got.TotalLength != 5 {
		t.Fatalf("unexpected bookkeeping: %+v", got)
	}
	if got.NextOffset != 0 {
		t.Fatalf("expected no continuation, got next offset %d", got.NextOffset)
	}
}

// TestSliceContentPaging: reading a section in slices must cover it exactly
// once, each slice pointing at the next one until the end.
func TestSliceContentPaging(t *testing.T) {
	const content = "abcdefghij"

	first := sliceContent(content, 0, 4)
	if first.Content != "abcd" || first.NextOffset != 4 {
		t.Fatalf("unexpected first slice: %+v", first)
	}

	second := sliceContent(content, first.NextOffset, 4)
	if second.Content != "efgh" || second.NextOffset != 8 {
		t.Fatalf("unexpected second slice: %+v", second)
	}

	last := sliceContent(content, second.NextOffset, 4)
	if last.Content != "ij" {
		t.Fatalf("unexpected last slice: %+v", last)
	}
	if last.NextOffset != 0 {
		t.Fatalf("expected the last slice to report no continuation, got %d", last.NextOffset)
	}
	if last.TotalLength != len(content) {
		t.Fatalf("expected the full length %d, got %d", len(content), last.TotalLength)
	}
}

// TestSliceContentCountsRunes: offsets are expressed in characters, so a slice
// never cuts a multi-byte rune in half.
func TestSliceContentCountsRunes(t *testing.T) {
	got := sliceContent("éàü", 1, 1)

	if got.Content != "à" {
		t.Fatalf("expected the second character, got %q", got.Content)
	}
	if got.TotalLength != 3 {
		t.Fatalf("expected a length of 3 characters, got %d", got.TotalLength)
	}
}

// TestSliceContentOutOfRange: an offset past the end is clamped rather than
// rejected, so a caller resuming from a stale offset learns the real size
// instead of getting an error.
func TestSliceContentOutOfRange(t *testing.T) {
	got := sliceContent("abc", 10, 5)

	if got.Content != "" || got.Length != 0 {
		t.Fatalf("expected an empty slice, got %+v", got)
	}
	if got.Offset != 3 || got.TotalLength != 3 {
		t.Fatalf("expected the offset clamped to the total length, got %+v", got)
	}
	if got.NextOffset != 0 {
		t.Fatalf("expected no continuation past the end, got %d", got.NextOffset)
	}

	if before := sliceContent("abc", -5, 0); before.Content != "abc" || before.Offset != 0 {
		t.Fatalf("expected a negative offset clamped to the beginning, got %+v", before)
	}
}

// TestShareBudgetUnbounded: without a budget, every section keeps its full
// length.
func TestShareBudgetUnbounded(t *testing.T) {
	lengths := []int{10, 200, 3000}

	if got := shareBudget(lengths, 0); !slices.Equal(got, lengths) {
		t.Fatalf("expected the full lengths %v, got %v", lengths, got)
	}
	if got := shareBudget(lengths, -1); !slices.Equal(got, lengths) {
		t.Fatalf("expected a negative budget to mean no bound, got %v", got)
	}
}

// TestShareBudgetFitsWithoutCutting: a response already under the budget is
// handed back whole — the bound must not cost anything when it does not bind.
func TestShareBudgetFitsWithoutCutting(t *testing.T) {
	lengths := []int{100, 200, 300}

	if got := shareBudget(lengths, 1000); !slices.Equal(got, lengths) {
		t.Fatalf("expected no cut under the budget, got %v", got)
	}
}

// TestShareBudgetServesWholeSectionsFirst: an excerpt is only useful whole, so
// the budget buys complete sections in relevance order rather than an equal
// stump of each — the ones it cannot afford come back empty and are fetched by
// ID.
func TestShareBudgetServesWholeSectionsFirst(t *testing.T) {
	// An even share would be 25 each: five tops of sections, no full one.
	got := shareBudget([]int{40, 30, 50, 20}, 100)

	if want := []int{40, 30, 30, 0}; !slices.Equal(got, want) {
		t.Fatalf("expected whole sections while the budget lasts %v, got %v", want, got)
	}
}

// TestShareBudgetStaysWithinBudget: whatever the mix of sizes, the sum of the
// allowances never exceeds the budget — that is what keeps the response under
// the client's cut.
func TestShareBudgetStaysWithinBudget(t *testing.T) {
	for _, lengths := range [][]int{
		{1, 1, 1},
		{7, 3000, 12, 900, 4},
		{5000, 5000, 5000},
	} {
		total := 0
		for _, n := range shareBudget(lengths, 33) {
			total += n
		}
		if total > 33 {
			t.Fatalf("expected at most 33 characters for %v, got %d", lengths, total)
		}
	}
}

// TestMaxContentCharsConfig: unset means the default budget, negative means no
// budget at all.
func TestMaxContentCharsConfig(t *testing.T) {
	var cfg config.Config

	if got := cfg.MaxContentChars(); got != config.DefaultMCPMaxContentChars {
		t.Fatalf("expected the default budget %d, got %d", config.DefaultMCPMaxContentChars, got)
	}

	cfg.MCP.MaxContentChars = -1
	if got := cfg.MaxContentChars(); got != 0 {
		t.Fatalf("expected a negative setting to mean no budget, got %d", got)
	}

	cfg.MCP.MaxContentChars = 500
	if got := cfg.MaxContentChars(); got != 500 {
		t.Fatalf("expected the configured budget, got %d", got)
	}
}

// TestTakeContentZeroLength: a section the budget could not afford comes back
// empty, and its total_length says it was not read — a zero length must not be
// read as "to the end".
func TestTakeContentZeroLength(t *testing.T) {
	got := takeContent("abc", 0, 0)

	if got.Content != "" {
		t.Fatalf("expected no content, got %q", got.Content)
	}
	if got.Length != 0 || got.TotalLength != 3 {
		t.Fatalf("expected a zero-length slice of a 3 character section, got %+v", got)
	}
}

// TestRenderResultsReportsOmittedSections: a trimmed result must never pass for
// an exhaustive one — the agent has to know there is more to fetch before it
// answers from an excerpt.
func TestRenderResultsReportsOmittedSections(t *testing.T) {
	ws, cfg := setupWorkspace(t)

	srv, err := New(t.Context(), ws, cfg)
	if err != nil {
		t.Fatalf("could not create MCP server: %+v", err)
	}
	defer srv.Close()

	source, parseErr := url.Parse("file:///doc.md")
	if parseErr != nil {
		t.Fatal(parseErr)
	}

	// Unknown section IDs are enough here: only the counting is under test, and
	// missing sections simply render without content.
	results := []*index.SearchResult{{
		Source:   source,
		Sections: sectionIDs("a", "b", "c", "d"),
	}}

	rendered, renderErr := srv.renderResults(t.Context(), results, 2)
	if renderErr != nil {
		t.Fatalf("could not render results: %+v", renderErr)
	}

	if len(rendered) != 1 {
		t.Fatalf("expected one rendered document, got %d", len(rendered))
	}
	if got := len(rendered[0].Sections); got != 2 {
		t.Fatalf("expected two sections returned, got %d", got)
	}
	if got := rendered[0].OmittedSections; got != 2 {
		t.Fatalf("expected two omitted sections reported, got %d", got)
	}
}
