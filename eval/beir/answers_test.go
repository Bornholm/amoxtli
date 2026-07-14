package beir

import "testing"

func TestLoadHotpotAnswers(t *testing.T) {
	dir := t.TempDir()
	// Native HotpotQA shape: array of objects with _id/question/answer (+ extra
	// fields the loader must ignore).
	path := writeFile(t, dir, "hotpot.json",
		`[
		  {"_id":"q1","question":"Who?","answer":"Barack Obama","type":"bridge"},
		  {"_id":"q2","question":"Both frozen?","answer":"no","supporting_facts":[]},
		  {"_id":"q3","question":"empty","answer":""}
		]`)

	answers, err := LoadHotpotAnswers(path)
	if err != nil {
		t.Fatalf("LoadHotpotAnswers: %v", err)
	}
	if len(answers) != 2 { // q3 dropped (empty answer)
		t.Fatalf("got %d answers, want 2", len(answers))
	}
	if got := answers["q1"]; len(got) != 1 || got[0] != "Barack Obama" {
		t.Errorf("q1 = %v, want [Barack Obama]", got)
	}
	if got := answers["q2"]; len(got) != 1 || got[0] != "no" {
		t.Errorf("q2 = %v, want [no]", got)
	}
	if _, ok := answers["q3"]; ok {
		t.Errorf("q3 with empty answer should be dropped")
	}
}
