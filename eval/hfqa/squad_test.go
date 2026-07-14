package hfqa

import (
	"strings"
	"testing"
)

// A minimal SQuAD-format document with two paragraphs and three questions, one
// of them unanswerable (SQuAD v2 style).
const sampleSQuAD = `{
  "version": "test",
  "data": [
    {
      "title": "Paris",
      "paragraphs": [
        {
          "context": "Paris est la capitale de la France.",
          "qas": [
            {"question": "Quelle est la capitale de la France ?", "id": "q1", "answers": [{"text": "Paris"}]},
            {"question": "Question impossible ?", "id": "q2", "is_impossible": true, "answers": []}
          ]
        },
        {
          "context": "La Seine traverse Paris.",
          "qas": [
            {"question": "Quel fleuve traverse Paris ?", "id": "q3", "answers": [{"text": "La Seine"}]}
          ]
        }
      ]
    }
  ]
}`

func TestReadSQuAD(t *testing.T) {
	corpus, dataset, err := Read(strings.NewReader(sampleSQuAD), "fr", "test-fr")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if len(corpus.Documents) != 2 {
		t.Fatalf("expected 2 documents, got %d", len(corpus.Documents))
	}
	for _, d := range corpus.Documents {
		if d.Lang != "fr" {
			t.Errorf("document lang = %q, want fr", d.Lang)
		}
		if d.Title != "Paris" {
			t.Errorf("document title = %q, want Paris", d.Title)
		}
		if !strings.HasPrefix(d.Source, "hfqa://fr/") {
			t.Errorf("unexpected source %q", d.Source)
		}
	}

	// The impossible question (q2) must be dropped: 2 answerable queries.
	if len(dataset.Queries) != 2 {
		t.Fatalf("expected 2 answerable queries, got %d", len(dataset.Queries))
	}

	// Each query's relevant source must point at an existing corpus document.
	sources := corpus.Sources()
	for _, q := range dataset.Queries {
		if q.Lang != "fr" {
			t.Errorf("query lang = %q, want fr", q.Lang)
		}
		if len(q.RelevantSources) != 1 {
			t.Fatalf("query %s has %d relevant sources", q.ID, len(q.RelevantSources))
		}
		if _, ok := sources[q.RelevantSources[0]]; !ok {
			t.Errorf("query %s points at unknown source %q", q.ID, q.RelevantSources[0])
		}
	}
}

func TestSourceIDStable(t *testing.T) {
	a := sourceID("fr", "Paris est la capitale de la France.")
	b := sourceID("fr", "Paris est la capitale de la France.")
	c := sourceID("en", "Paris est la capitale de la France.")
	if a != b {
		t.Errorf("sourceID not stable: %q != %q", a, b)
	}
	if a == c {
		t.Errorf("sourceID should differ by language")
	}
}
