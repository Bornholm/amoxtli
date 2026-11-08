package index

import (
	"testing"

	"github.com/bornholm/amoxtli/model"
)

func TestAggregateHitsRanksByBestSection(t *testing.T) {
	hits := []Hit{
		// A long document matching a little in many sections.
		{Source: "doc:long", Section: "l1", Score: 1},
		{Source: "doc:long", Section: "l2", Score: 1},
		{Source: "doc:long", Section: "l3", Score: 1},
		// A short document with one section that matches well, reached
		// through two of its chunks.
		{Source: "doc:short", Section: "s1", Score: 2},
		{Source: "doc:short", Section: "s1", Score: 1.5},
		{Source: "doc:short", Section: "s2", Score: 0.5},
		{Source: "doc:other", Section: "o1", Score: 0.1},
	}

	results, err := AggregateHits(hits, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if got := results[0].Source.String(); got != "doc:short" {
		t.Fatalf("expected doc:short first, got %s", got)
	}
	if results[0].Score != 2 {
		t.Errorf("expected score 2, got %v", results[0].Score)
	}
	if want := []model.SectionID{"s1", "s2"}; len(results[0].Sections) != 2 || results[0].Sections[0] != want[0] || results[0].Sections[1] != want[1] {
		t.Errorf("expected sections %v without duplicates, got %v", want, results[0].Sections)
	}
	if results[0].SectionScores["s1"] != 2 {
		t.Errorf("expected section s1 to keep its best chunk score, got %v", results[0].SectionScores["s1"])
	}
}
