package retrieval

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
)

// lexicalStore builds a store from id → content pairs, in declaration order.
func lexicalStore(pairs ...string) *stubStore {
	sections := map[model.SectionID]model.Section{}
	for i := 0; i < len(pairs); i += 2 {
		id := model.SectionID(pairs[i])
		sections[id] = &stubSection{id: id, content: pairs[i+1]}
	}
	return &stubStore{sections: sections}
}

// rerankedOrder returns the section IDs in their reranked order, one per result.
func rerankedOrder(t *testing.T, out []*index.SearchResult) []string {
	t.Helper()
	order := make([]string, 0, len(out))
	for _, r := range out {
		if len(r.Sections) == 0 {
			t.Fatalf("result %s came back with no section", r.Source)
		}
		order = append(order, string(r.Sections[0]))
	}
	return order
}

// TestLexicalRerankerPromotesFullCoverage pins the signal RRF cannot express.
// Every candidate arrives with the same fused prior, so the fusion has no
// opinion; only the content can separate them, and the section answering the
// whole query must come out first. It runs on the default weights, so the
// separation comes from BM25 with pool-level IDF alone.
func TestLexicalRerankerPromotesFullCoverage(t *testing.T) {
	store := lexicalStore(
		"partial", "mitochondrial dysfunction is widely studied in many tissues",
		"full", "mitochondrial dysfunction impairs cardiac muscle contraction",
		"unrelated", "the treaty was signed in vienna during the spring",
	)

	results := []*index.SearchResult{
		resultForSource("docPartial", "partial"),
		resultForSource("docFull", "full"),
		resultForSource("docUnrelated", "unrelated"),
	}

	out, err := NewLexicalReranker(store).Rerank(context.Background(), "mitochondrial dysfunction cardiac muscle", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	got := rerankedOrder(t, out)
	if got[0] != "full" {
		t.Errorf("reranked order = %v, want the fully-covering section first", got)
	}
	if got[len(got)-1] != "unrelated" {
		t.Errorf("reranked order = %v, want the unrelated section last", got)
	}
}

// TestLexicalRerankerRewardsProximity isolates the proximity feature: both
// sections cover the query entirely and are the same length, so every other
// signal is tied and only the clustering of the terms can break it.
func TestLexicalRerankerRewardsProximity(t *testing.T) {
	filler := strings.Repeat("filler ", 20)

	store := lexicalStore(
		"scattered", "cardiac "+filler+"arrest "+filler+"survival",
		"tight", "cardiac arrest survival "+filler+filler,
	)

	results := []*index.SearchResult{
		resultForSource("docScattered", "scattered"),
		resultForSource("docTight", "tight"),
	}

	// Proximity is off by default (see DefaultLexicalWeights), so the test turns
	// it on: prior and BM25 are tied across the pool — same terms, same
	// frequencies, same length — and normalise away, leaving proximity as the
	// only signal that can decide.
	weights := DefaultLexicalWeights
	weights.Proximity = 1

	out, err := NewLexicalReranker(store, WithLexicalWeights(weights)).
		Rerank(context.Background(), "cardiac arrest survival", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := rerankedOrder(t, out); got[0] != "tight" {
		t.Errorf("reranked order = %v, want the section with clustered terms first", got)
	}
}

// TestLexicalRerankerIgnoresPoolWideTerms is the reason no stopword list is
// needed: a term carried by every candidate has an IDF of zero, so repeating it
// cannot buy rank.
func TestLexicalRerankerIgnoresPoolWideTerms(t *testing.T) {
	store := lexicalStore(
		"spammy", "study study study study study study study study of enzymes",
		"relevant", "study of protein folding kinetics",
	)

	results := []*index.SearchResult{
		resultForSource("docSpammy", "spammy"),
		resultForSource("docRelevant", "relevant"),
	}

	out, err := NewLexicalReranker(store).Rerank(context.Background(), "study of protein folding", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := rerankedOrder(t, out); got[0] != "relevant" {
		t.Errorf("reranked order = %v, want the section matching the informative terms first", got)
	}
}

// TestLexicalRerankerHonorsPriorWhenContentTies guards against the reranker
// discarding the retrieval consensus: with identical content, the order must be
// the fused one.
func TestLexicalRerankerHonorsPriorWhenContentTies(t *testing.T) {
	store := lexicalStore(
		"low", "identical content about photosynthesis",
		"high", "identical content about photosynthesis",
	)

	low := resultForSource("docLow", "low")
	low.Score = 0.01
	high := resultForSource("docHigh", "high")
	high.Score = 0.9

	// Fused order deliberately puts the weaker candidate first: only the prior
	// can restore the intended ranking.
	out, err := NewLexicalReranker(store).Rerank(context.Background(), "photosynthesis", []*index.SearchResult{low, high})
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := rerankedOrder(t, out); got[0] != "high" {
		t.Errorf("reranked order = %v, want the higher fused prior first", got)
	}
}

// TestLexicalRerankerIsDeterministic covers the property pagination cursors rely
// on: the same pool must always produce the same order, including when scores
// tie exactly.
func TestLexicalRerankerIsDeterministic(t *testing.T) {
	store := lexicalStore(
		"a", "quantum entanglement experiment",
		"b", "quantum entanglement experiment",
		"c", "quantum entanglement experiment",
	)

	build := func() []*index.SearchResult {
		return []*index.SearchResult{
			resultForSource("docA", "a"),
			resultForSource("docB", "b"),
			resultForSource("docC", "c"),
		}
	}

	reranker := NewLexicalReranker(store)

	first, err := reranker.Rerank(context.Background(), "quantum entanglement", build())
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	for i := range 10 {
		next, err := reranker.Rerank(context.Background(), "quantum entanglement", build())
		if err != nil {
			t.Fatalf("unexpected error: %+v", err)
		}
		if got, want := rerankedOrder(t, next), rerankedOrder(t, first); !equalStrings(got, want) {
			t.Fatalf("run %d reordered a tied pool: %v, want %v", i, got, want)
		}
	}
}

// TestLexicalRerankerKeepsUnknownSections covers a section deleted between the
// search and the rerank: it must not be dropped from the results, only left
// unscored.
func TestLexicalRerankerKeepsUnknownSections(t *testing.T) {
	store := lexicalStore("known", "neural network training dynamics")

	results := []*index.SearchResult{
		resultForSource("docGhost", "ghost"),
		resultForSource("docKnown", "known"),
	}

	out, err := NewLexicalReranker(store).Rerank(context.Background(), "neural network", results)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if len(out) != 2 {
		t.Fatalf("got %d results, want the 2 inputs preserved", len(out))
	}
	if got := rerankedOrder(t, out); got[0] != "known" {
		t.Errorf("reranked order = %v, want the scorable section first", got)
	}
}

func TestLexicalRerankerShortCircuits(t *testing.T) {
	store := lexicalStore("a", "some content")

	single := []*index.SearchResult{resultForSource("docA", "a")}
	out, err := NewLexicalReranker(store).Rerank(context.Background(), "content", single)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d results, want 1", len(out))
	}

	// A query with no term at all leaves the fused order untouched rather than
	// scoring every candidate at zero and reordering them arbitrarily.
	pair := []*index.SearchResult{resultForSource("docA", "a"), resultForSource("docB", "a")}
	out, err = NewLexicalReranker(store).Rerank(context.Background(), "!?...", pair)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if out[0].Source.Host != "docA" || out[1].Source.Host != "docB" {
		t.Errorf("termless query reordered the results: %v", rerankedOrder(t, out))
	}
}

// --- feature-level tests ---------------------------------------------------

func TestProximityScoreOrdersByTightness(t *testing.T) {
	tight := &scoredSection{termPositions: [][]int{{0}, {1}, {2}}}
	loose := &scoredSection{termPositions: [][]int{{0}, {50}, {100}}}

	if got, want := proximityScore(tight, 3), proximityScore(loose, 3); got <= want {
		t.Errorf("tight proximity = %.4f, loose = %.4f: want tight strictly greater", got, want)
	}
}

// A window covering the whole query must beat a tighter one covering half of
// it — otherwise the feature would reward matching a single bigram and ignoring
// the rest of the question.
func TestProximityScorePrefersFullCoverage(t *testing.T) {
	full := &scoredSection{termPositions: [][]int{{0}, {2}, {4}, {6}}}
	partial := &scoredSection{termPositions: [][]int{{0}, {1}, nil, nil}}

	if got, want := proximityScore(full, 4), proximityScore(partial, 4); got <= want {
		t.Errorf("full-coverage proximity = %.4f, partial = %.4f: want full strictly greater", got, want)
	}
}

func TestProximityScoreHandlesNoMatch(t *testing.T) {
	if got := proximityScore(&scoredSection{termPositions: [][]int{nil, nil, nil}}, 3); got != 0 {
		t.Errorf("proximity with no matched term = %.4f, want 0", got)
	}
}

// TestPoolIDFDiscountsUbiquitousTerms is the reason no stopword list is needed.
// Slot 0 stands for a term present in every candidate, slot 1 for one present
// in a single candidate.
func TestPoolIDFDiscountsUbiquitousTerms(t *testing.T) {
	docs := []*scoredSection{
		{termPositions: [][]int{{0}, {1}}},
		{termPositions: [][]int{{0}, nil}},
		{termPositions: [][]int{{0}, nil}},
		{termPositions: [][]int{{0}, nil}},
	}

	idf := poolIDF(docs, 2)

	if idf[1] <= 0 {
		t.Fatalf("IDF of a term present in one candidate = %.4f, want > 0", idf[1])
	}
	// The BM25 IDF form never reaches exactly zero, so what matters is the
	// ratio: a ubiquitous term must be worth an order of magnitude less than a
	// discriminating one.
	if ratio := idf[0] / idf[1]; ratio > 0.1 {
		t.Errorf("ubiquitous/rare IDF ratio = %.4f (%.4f vs %.4f), want a term present everywhere to be near-worthless",
			ratio, idf[0], idf[1])
	}
}

// TestPoolIDFNeverGoesNegative pins the clamp: without it a term present in
// more than half the pool gets a negative BM25 IDF, and a section containing it
// would be *penalised* for matching the query.
func TestPoolIDFNeverGoesNegative(t *testing.T) {
	docs := make([]*scoredSection, 100)
	for i := range docs {
		docs[i] = &scoredSection{termPositions: [][]int{{0}}}
	}

	if got := poolIDF(docs, 1)[0]; got < 0 {
		t.Errorf("IDF = %.4f, want it clamped at 0", got)
	}
}

func TestNormalizeSliceFlattensAConstantPool(t *testing.T) {
	// A feature that does not discriminate must not shift every score by a
	// constant: it must contribute nothing at all.
	values := []float64{3, 3, 3}
	normalizeSlice(values)
	for i, v := range values {
		if v != 0 {
			t.Errorf("normalized[%d] = %.4f, want 0 for a constant pool", i, v)
		}
	}

	values = []float64{1, 3, 5}
	normalizeSlice(values)
	for i, want := range []float64{0, 0.5, 1} {
		if math.Abs(values[i]-want) > 1e-9 {
			t.Errorf("normalized[%d] = %.4f, want %.4f", i, values[i], want)
		}
	}
}

func TestNormalizeTextCollapsesPunctuation(t *testing.T) {
	if got, want := normalizeText("  Héllo, WORLD! -- 42  "), "héllo world 42"; got != want {
		t.Errorf("normalizeText = %q, want %q", got, want)
	}
	if got := normalizeText("!?..."); got != "" {
		t.Errorf("normalizeText of punctuation only = %q, want empty", got)
	}
}

// TestSimpleTokenizerAgreesWithNormalizeText pins the invariant the two paths
// share: SimpleTokenizer splits the raw content on the hot path without
// materialising it, normalizeText materialises it for phrase matching. If they
// ever disagreed on what a token is, a phrase match would be checked against a
// different tokenisation than the one the BM25 and proximity features saw.
func TestSimpleTokenizerAgreesWithNormalizeText(t *testing.T) {
	raw := "Mitochondrial dysfunction, in CARDIAC muscle -- 42 fois; l'ADN."

	want := strings.Fields(normalizeText(raw))

	slots := make(map[string]int, len(want))
	for i, term := range uniqueTerms(want) {
		slots[term] = i
	}
	positions := make([][]int, len(slots))

	r := NewLexicalReranker(nil)
	if got := r.scanInto(raw, slots, positions); got != len(want) {
		t.Errorf("the tokenizer counted %d tokens, normalizeText yields %d (%v)", got, len(want), want)
	}

	// Offsets must line up too, not just the count.
	for term, slot := range slots {
		var expected []int
		for i, tok := range want {
			if tok == term {
				expected = append(expected, i)
			}
		}
		if len(positions[slot]) != len(expected) {
			t.Errorf("term %q: scanned positions %v, want %v", term, positions[slot], expected)
			continue
		}
		for i := range expected {
			if positions[slot][i] != expected[i] {
				t.Errorf("term %q: scanned positions %v, want %v", term, positions[slot], expected)
				break
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var _ = url.URL{}

// BenchmarkLexicalReranker measures a pessimistic rerank window: 100 candidate
// sections of ~300 words against a 12-term query. The default window is
// max(pageSize*4, 20), so a five-result page reranks twenty sections and costs
// roughly a fifth of this. It is the number that justifies the component's
// existence next to the LLM reranker, whose equivalent is a network round-trip.
func BenchmarkLexicalReranker(b *testing.B) {
	const (
		candidates  = 100
		wordsPerSec = 300
	)

	vocabulary := []string{
		"mitochondrial", "dysfunction", "cardiac", "muscle", "contraction", "protein",
		"folding", "kinetics", "enzyme", "substrate", "receptor", "binding", "affinity",
		"cellular", "membrane", "transport", "oxidative", "stress", "apoptosis", "signalling",
	}

	sections := map[model.SectionID]model.Section{}
	results := make([]*index.SearchResult, 0, candidates)

	for i := range candidates {
		var sb strings.Builder
		for w := range wordsPerSec {
			sb.WriteString(vocabulary[(i*7+w*13)%len(vocabulary)])
			sb.WriteByte(' ')
		}

		id := model.SectionID("sec-" + strconv.Itoa(i))
		sections[id] = &stubSection{id: id, content: sb.String()}

		res := resultForSource("doc-"+strconv.Itoa(i), id)
		res.Score = 1 / float64(i+1)
		results = append(results, res)
	}

	query := strings.Join(vocabulary[:12], " ")
	ctx := context.Background()

	// Phrase matching is the only feature forcing a normalised copy of every
	// section, so it is benchmarked separately: it is off by default, and a
	// deployment considering turning it on should know what it costs.
	withPhrase := DefaultLexicalWeights
	withPhrase.Phrase = 0.25

	for _, variant := range []struct {
		name    string
		weights LexicalWeights
	}{
		{"default", DefaultLexicalWeights},
		{"with-phrase", withPhrase},
	} {
		b.Run(variant.name, func(b *testing.B) {
			reranker := NewLexicalReranker(&stubStore{sections: sections}, WithLexicalWeights(variant.weights))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := reranker.Rerank(ctx, query, results); err != nil {
					b.Fatalf("unexpected error: %+v", err)
				}
			}
		})
	}
}

// TestLexicalRerankerPhraseIsOptInAndWorks covers both halves of the phrase
// signal: it is off by default — it is the one feature that materialises the
// section text, and it earned nothing on SciFact — and it decides the ordering
// once enabled.
func TestLexicalRerankerPhraseIsOptInAndWorks(t *testing.T) {
	// Same terms, same frequencies, same length: only word order differs, so
	// every other signal ties and normalises away.
	store := lexicalStore(
		"shuffled", "pressure blood high measurement",
		"verbatim", "high blood pressure measurement",
	)

	build := func() []*index.SearchResult {
		return []*index.SearchResult{
			resultForSource("docShuffled", "shuffled"),
			resultForSource("docVerbatim", "verbatim"),
		}
	}

	// Off by default: the tied pool keeps the fused order, shuffled first.
	out, err := NewLexicalReranker(store).Rerank(context.Background(), "high blood pressure", build())
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := rerankedOrder(t, out); got[0] != "shuffled" {
		t.Errorf("with phrase off, order = %v, want the fused order preserved", got)
	}

	weights := DefaultLexicalWeights
	weights.Phrase = 1

	out, err = NewLexicalReranker(store, WithLexicalWeights(weights)).
		Rerank(context.Background(), "high blood pressure", build())
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	if got := rerankedOrder(t, out); got[0] != "verbatim" {
		t.Errorf("with phrase on, order = %v, want the verbatim occurrence first", got)
	}
}
