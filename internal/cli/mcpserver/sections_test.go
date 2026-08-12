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
