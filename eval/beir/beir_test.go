package beir

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

func TestLoadBEIR(t *testing.T) {
	dir := t.TempDir()

	corpus := writeFile(t, dir, "corpus.jsonl",
		`{"_id":"d1","title":"Cats","text":"Felines are small carnivorous mammals."}`+"\n"+
			`{"_id":"d2","title":"Dogs","text":"Canines are loyal domesticated animals."}`+"\n"+
			`{"_id":"d3","title":"Cars","text":"Automobiles are wheeled motor vehicles."}`+"\n")
	queries := writeFile(t, dir, "queries.jsonl",
		`{"_id":"q1","text":"information about kittens and cats"}`+"\n"+
			`{"_id":"q2","text":"loyal pets that bark"}`+"\n"+
			`{"_id":"q3","text":"query with no qrel"}`+"\n")
	qrels := writeFile(t, dir, "test.tsv",
		"query-id\tcorpus-id\tscore\n"+
			"q1\td1\t1\n"+
			"q2\td2\t2\n"+
			"q2\td99\t1\n") // d99 not in corpus → dropped

	c, ds, err := Load(corpus, queries, qrels, "toy", "en")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(c.Documents) != 3 {
		t.Fatalf("corpus size = %d, want 3", len(c.Documents))
	}
	// q3 has no qrel → dropped; q1 and q2 kept.
	if len(ds.Queries) != 2 {
		t.Fatalf("queries = %d, want 2", len(ds.Queries))
	}

	byID := map[string]int{}
	for i, q := range ds.Queries {
		byID[q.ID] = i
	}
	q1, ok := byID["q1"]
	if !ok {
		t.Fatal("q1 missing")
	}
	if got := ds.Queries[q1].RelevantSources; len(got) != 1 || got[0] != Source("toy", "d1") {
		t.Errorf("q1 relevant = %v, want [%s]", got, Source("toy", "d1"))
	}
	// q2: d2 kept (in corpus), d99 dropped (absent).
	q2 := byID["q2"]
	if got := ds.Queries[q2].RelevantSources; len(got) != 1 || got[0] != Source("toy", "d2") {
		t.Errorf("q2 relevant = %v, want [%s]", got, Source("toy", "d2"))
	}
	if ds.Queries[q2].Lang != "en" {
		t.Errorf("q2 lang = %q, want en", ds.Queries[q2].Lang)
	}
}

func TestLoadSubsampleStreaming(t *testing.T) {
	dir := t.TempDir()

	// Gold docs (g*) are scattered among many distractors (n*), as in a large
	// BEIR corpus where relevant documents are not up front.
	corpus := writeFile(t, dir, "corpus.jsonl",
		`{"_id":"n1","title":"","text":"distractor one"}`+"\n"+
			`{"_id":"g2","title":"","text":"gold for q2"}`+"\n"+
			`{"_id":"n2","title":"","text":"distractor two"}`+"\n"+
			`{"_id":"g1","title":"","text":"gold for q1"}`+"\n"+
			`{"_id":"n3","title":"","text":"distractor three"}`+"\n"+
			`{"_id":"g3","title":"","text":"gold for q3"}`+"\n"+
			`{"_id":"n4","title":"","text":"distractor four"}`+"\n")
	queries := writeFile(t, dir, "queries.jsonl",
		`{"_id":"q1","text":"first"}`+"\n"+
			`{"_id":"q2","text":"second"}`+"\n"+
			`{"_id":"q3","text":"third"}`+"\n"+
			`{"_id":"q4","text":"missing gold"}`+"\n")
	qrels := writeFile(t, dir, "test.tsv",
		"query-id\tcorpus-id\tscore\n"+
			"q1\tg1\t1\n"+
			"q2\tg2\t1\n"+
			"q3\tg3\t1\n"+
			"q4\tg99\t1\n") // g99 absent from corpus → q4 unanswerable

	// maxQueries=2 → deterministic first two by sorted id (q1, q2). Their gold is
	// {g1, g2}. maxDocs=3 → budget of 1 distractor on top of the 2 gold docs.
	c, ds, err := LoadSubsample(corpus, queries, qrels, "toy", "en", 2, 3)
	if err != nil {
		t.Fatalf("LoadSubsample: %v", err)
	}

	if len(ds.Queries) != 2 {
		t.Fatalf("queries = %d, want 2 (q1,q2)", len(ds.Queries))
	}
	if ds.Queries[0].ID != "q1" || ds.Queries[1].ID != "q2" {
		t.Errorf("queries = %s,%s, want q1,q2", ds.Queries[0].ID, ds.Queries[1].ID)
	}

	got := map[string]struct{}{}
	for _, d := range c.Documents {
		got[d.Source] = struct{}{}
	}
	// Both gold docs must be present regardless of their position in the file.
	for _, id := range []string{"g1", "g2"} {
		if _, ok := got[Source("toy", id)]; !ok {
			t.Errorf("gold %s missing from streamed corpus", id)
		}
	}
	// Total capped at maxDocs=3 (2 gold + 1 distractor).
	if len(c.Documents) != 3 {
		t.Fatalf("corpus size = %d, want 3", len(c.Documents))
	}
	// The single distractor is the first non-gold in file order (n1).
	if _, ok := got[Source("toy", "n1")]; !ok {
		t.Errorf("distractor n1 (first in file order) missing")
	}
}

func TestLoadSubsampleGoldExceedsBudget(t *testing.T) {
	dir := t.TempDir()

	// A query with two gold docs and maxDocs=1: gold is always kept even beyond
	// the budget, and the budget leaves room for zero distractors.
	corpus := writeFile(t, dir, "corpus.jsonl",
		`{"_id":"n1","title":"","text":"distractor"}`+"\n"+
			`{"_id":"g1","title":"","text":"gold a"}`+"\n"+
			`{"_id":"g2","title":"","text":"gold b"}`+"\n")
	queries := writeFile(t, dir, "queries.jsonl", `{"_id":"q1","text":"multi"}`+"\n")
	qrels := writeFile(t, dir, "test.tsv",
		"query-id\tcorpus-id\tscore\ng1\tignored\t0\n"+ // header sanity, score 0 skipped
			"q1\tg1\t1\nq1\tg2\t1\n")

	c, ds, err := LoadSubsample(corpus, queries, qrels, "toy", "", 0, 1)
	if err != nil {
		t.Fatalf("LoadSubsample: %v", err)
	}
	if len(ds.Queries) != 1 {
		t.Fatalf("queries = %d, want 1", len(ds.Queries))
	}
	if len(c.Documents) != 2 {
		t.Fatalf("corpus size = %d, want 2 (both gold, no distractor)", len(c.Documents))
	}
	src := map[string]struct{}{}
	for _, d := range c.Documents {
		src[d.Source] = struct{}{}
	}
	if _, ok := src[Source("toy", "n1")]; ok {
		t.Errorf("distractor n1 kept despite zero budget")
	}
}
