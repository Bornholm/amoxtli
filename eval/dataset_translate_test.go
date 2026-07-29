package eval

import "testing"

func TestDatasetTranslateQueries(t *testing.T) {
	ds := &Dataset{
		Name: "scifact",
		Queries: []Query{
			{ID: "1", Query: "lexical channel", Lang: "en", RelevantSources: []string{"a"}, Answers: []string{"gold"}, Tags: []string{"ir"}},
			{ID: "2", Query: "semantic channel", Lang: "en", RelevantSources: []string{"b"}},
		},
	}

	translated, count := ds.TranslateQueries(map[string]string{"1": "canal lexical"}, "fr")

	if e, g := 1, count; e != g {
		t.Errorf("translated count: expected %d, got %d", e, g)
	}

	// The translated query carries the new text and language...
	if e, g := "canal lexical", translated.Queries[0].Query; e != g {
		t.Errorf("Queries[0].Query: expected %q, got %q", e, g)
	}
	if e, g := "fr", translated.Queries[0].Lang; e != g {
		t.Errorf("Queries[0].Lang: expected %q, got %q", e, g)
	}

	// ...and everything the gold set is made of survives untouched, since the
	// two runs are only comparable if they are scored against the same truth.
	if e, g := "a", translated.Queries[0].RelevantSources[0]; e != g {
		t.Errorf("Queries[0].RelevantSources[0]: expected %q, got %q", e, g)
	}
	if e, g := "gold", translated.Queries[0].Answers[0]; e != g {
		t.Errorf("Queries[0].Answers[0]: expected %q, got %q", e, g)
	}
	if e, g := "ir", translated.Queries[0].Tags[0]; e != g {
		t.Errorf("Queries[0].Tags[0]: expected %q, got %q", e, g)
	}

	// An untranslated query is kept verbatim rather than dropped.
	if e, g := "semantic channel", translated.Queries[1].Query; e != g {
		t.Errorf("Queries[1].Query: expected %q, got %q", e, g)
	}
	if e, g := "en", translated.Queries[1].Lang; e != g {
		t.Errorf("Queries[1].Lang: expected %q, got %q", e, g)
	}

	// The source dataset must not be mutated: the baseline run happens after.
	if e, g := "lexical channel", ds.Queries[0].Query; e != g {
		t.Errorf("source dataset was mutated: Queries[0].Query = %q, want %q", g, e)
	}
}
