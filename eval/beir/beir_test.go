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
