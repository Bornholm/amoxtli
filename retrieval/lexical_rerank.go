package retrieval

import (
	"context"
	"log/slog"
	"math"
	"slices"
	"strings"
	"unicode"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/telemetry"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// LexicalReranker reorders fused search results using only signals computable
// from the query and the text already retrieved: no model, no network call, no
// GPU. It is the cheap counterpart of LLMReranker — single-digit milliseconds
// for a full rerank window instead of an LLM round-trip — and implements the
// same ingest.Reranker contract, so the two are interchangeable.
//
// It exists because Reciprocal Rank Fusion, which produces the results it is
// given, is deliberately blind to score magnitude: a section retrieved at rank 1
// by the vector leg contributes exactly as much as a lexically perfect match at
// rank 1 from the full-text leg. Rescoring the candidates against their own
// content puts every leg back on a single, comparable scale, and adds two
// signals a bag-of-words retriever does not have at all: how tightly the query
// terms cluster in the text, and whether the query appears verbatim.
//
// The document frequencies used for IDF are computed over the candidate pool
// rather than the whole corpus. That is an approximation, but a self-correcting
// one: a term present in every candidate carries no information about which
// candidate is better, and gets an IDF close to zero — which is why no
// language-specific stopword list is needed.
type LexicalReranker struct {
	store     SectionStore
	weights   LexicalWeights
	tokenizer Tokenizer
}

// LexicalWeights are the relative contributions of each signal to the final
// section score. Every feature is bounded in [0,1] before weighting, so the
// weights are directly comparable to one another.
type LexicalWeights struct {
	// Prior is the weight of the incoming fused (RRF) score. Keeping it non-zero
	// means the reranker refines the retrieval consensus rather than replacing
	// it — the safe default, since the fusion has corpus-wide knowledge the
	// reranker does not.
	Prior float64
	// BM25 weights the Okapi BM25 score of the section against the query, with
	// pool-level IDF.
	BM25 float64
	// Coverage weights the IDF-weighted fraction of distinct query terms present
	// in the section. BM25 alone can rank a section repeating one rare term above
	// a section answering the whole query; this is the corrective.
	Coverage float64
	// Proximity weights how tightly the matched query terms cluster: the ratio of
	// distinct terms found to the length of the smallest window containing them.
	Proximity float64
	// Phrase weights an exact, verbatim occurrence of the query in the section.
	// It is the only signal requiring the section text to be materialised, so
	// leaving it at zero measurably lowers the reranker's cost.
	Phrase float64
}

// DefaultLexicalWeights weighs the retrieval consensus and BM25 equally and
// leaves the three corrective signals off.
//
// That is a measured choice, not a conservative guess. On BEIR SciFact (300
// queries, 1000 documents, hybrid bleve + vector retrieval) the ablation reads:
//
//	prior + bm25                       nDCG@10 0.780   MRR 0.757
//	prior + bm25 + cov + prox + phrase nDCG@10 0.777   MRR 0.755
//	prior only (bm25 off)              nDCG@10 0.692   MRR 0.645
//	bm25 only (prior off)              nDCG@10 0.761   MRR 0.737
//
// Coverage, proximity and phrase are all functions of the same term-position
// data BM25 already consumes, and on that corpus they turned out to carry no
// information it had not extracted — while Phrase alone makes the reranker
// roughly half as fast again (6.7ms → 10.2ms on the benchmark window), being
// the one signal that materialises the section text.
//
// They are kept implemented rather than deleted because a single dataset does
// not settle it: SciFact is English, one short abstract per document. A corpus
// of long documents, or queries where word order carries meaning, is exactly
// where proximity and phrase should earn their weight. Turn them on with
// WithLexicalWeights and measure on your own corpus before trusting either
// default.
var DefaultLexicalWeights = LexicalWeights{
	Prior:     1.0,
	BM25:      1.0,
	Coverage:  0,
	Proximity: 0,
	Phrase:    0,
}

// bm25K1 and bm25B are the standard Okapi BM25 parameters: k1 bounds term
// frequency saturation, b the strength of the length normalisation.
const (
	bm25K1 = 1.2
	bm25B  = 0.75
)

// Rerank implements ingest.Reranker.
func (r *LexicalReranker) Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error) {
	if len(results) <= 1 {
		return results, nil
	}

	queryTerms := uniqueTerms(r.scanTerms(query))
	if len(queryTerms) == 0 {
		// Nothing to match on (a query of punctuation, or of nothing but
		// separators): the fused order is the best information available.
		return results, nil
	}

	ctx, span := telemetry.Tracer().Start(ctx, "retrieval.lexical_rerank",
		trace.WithAttributes(attribute.Int(telemetry.AttrCandidateCount, len(results))),
	)
	defer span.End()

	sections, err := r.store.GetSectionsByIDs(ctx, allSectionIDs(results))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	span.SetAttributes(attribute.Int(telemetry.AttrSectionCount, len(sections)))

	// A phrase match is only meaningful for a multi-word query — for a single
	// term it would duplicate coverage exactly — and it is the one feature that
	// forces a normalised copy of every section. Decide once, here, so the scan
	// below can skip that copy entirely.
	phraseQuery := ""
	if r.weights.Phrase != 0 && len(queryTerms) > 1 {
		// Phrase matching stays on the raw surface form deliberately: it asks
		// whether the query appears *verbatim*, a question stemming would erase.
		phraseQuery = normalizeText(query)
	}

	docs, err := r.buildDocuments(results, sections, queryTerms, phraseQuery != "")
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(docs) == 0 {
		return results, nil
	}

	scoreDocuments(docs, queryTerms, phraseQuery, r.weights)

	slog.DebugContext(ctx, "lexical reranker scored sections",
		slog.Int("sections", len(docs)),
		slog.Int("queryTerms", len(queryTerms)),
	)

	return applyRanking(results, rankFromScores(docs)), nil
}

// scoredSection is one candidate section with its extracted features. Features
// are collected first and combined afterwards, because two of them (IDF, BM25
// length normalisation) are relative to the pool.
type scoredSection struct {
	id model.SectionID
	// termPositions holds, per query term (by its index in the query term
	// slice), the token offsets at which it occurs. A slice rather than a map:
	// it is written once per matched token on the hot path, and the query term
	// set is fixed and small.
	termPositions [][]int
	tokenCount    int
	// text is the normalised section content, materialised only when phrase
	// matching is enabled.
	text  string
	prior float64

	bm25      float64
	coverage  float64
	proximity float64
	phrase    float64
	score     float64
}

// matched reports the number of distinct query terms present in the section.
func (s *scoredSection) matched() int {
	distinct := 0
	for _, positions := range s.termPositions {
		if len(positions) > 0 {
			distinct++
		}
	}
	return distinct
}

// buildDocuments hydrates and scans every candidate section once, recording the
// per-term positions the features are derived from.
func (r *LexicalReranker) buildDocuments(results []*index.SearchResult, sections map[model.SectionID]model.Section, queryTerms []string, withText bool) ([]*scoredSection, error) {
	// Term → slot, so the scan can resolve a token to its position bucket with a
	// single map read keyed by a byte slice — which the compiler performs
	// without allocating a string, unlike a map write would.
	slots := make(map[string]int, len(queryTerms))
	for i, t := range queryTerms {
		slots[t] = i
	}

	docs := make([]*scoredSection, 0, len(sections))
	seen := make(map[model.SectionID]struct{}, len(sections))

	for _, res := range results {
		for _, id := range res.Sections {
			if _, done := seen[id]; done {
				continue
			}
			seen[id] = struct{}{}

			section, exists := sections[id]
			if !exists {
				// The store no longer knows this section (deleted between the
				// search and the rerank): it keeps its fused rank rather than
				// being scored against content that cannot be read.
				continue
			}

			content, err := section.Content()
			if err != nil {
				return nil, errors.WithStack(err)
			}

			doc := &scoredSection{
				id:            id,
				termPositions: make([][]int, len(queryTerms)),
				// The prior is the result's fused score, not its SectionScores
				// entry. The latter looks like the finer-grained signal but is
				// not comparable across documents: the pipeline builds it from
				// each section's rank *within its own source*, so the leading
				// section of every result carries the same value whether the
				// result is excellent or barely relevant. Ranking documents
				// against one another with it measurably degrades the ordering.
				prior: res.Score,
			}
			if withText {
				doc.text = normalizeText(string(content))
			}

			doc.tokenCount = r.scanInto(string(content), slots, doc.termPositions)

			docs = append(docs, doc)
		}
	}

	return docs, nil
}

// scanTerms returns the distinct-ready term list of a short text (a query),
// materialised as strings. It is only used on queries, which are small — the
// section path below stays allocation-free.
func (r *LexicalReranker) scanTerms(text string) []string {
	var terms []string
	r.tokenizer.Scan(text, func(_ int, term []byte) {
		terms = append(terms, string(term))
	})
	return terms
}

// scanInto walks a section once and appends the position of every token
// belonging to the query to its slot, returning the total token count.
//
// The map read keyed by string(term) does not copy the bytes, which is what
// makes this loop allocation-free and why Tokenizer hands out a borrowed slice.
func (r *LexicalReranker) scanInto(content string, slots map[string]int, positions [][]int) int {
	count := 0

	r.tokenizer.Scan(content, func(position int, term []byte) {
		count++
		if slot, ok := slots[string(term)]; ok {
			positions[slot] = append(positions[slot], position)
		}
	})

	return count
}

// scoreDocuments extracts every feature, normalises the pool-relative ones and
// combines them into the final per-section score.
func scoreDocuments(docs []*scoredSection, queryTerms []string, phraseQuery string, w LexicalWeights) {
	idf := poolIDF(docs, len(queryTerms))

	var totalTokens int
	for _, d := range docs {
		totalTokens += d.tokenCount
	}
	avgLen := float64(totalTokens) / float64(len(docs))
	if avgLen == 0 {
		avgLen = 1
	}

	var totalIDF float64
	for _, v := range idf {
		totalIDF += v
	}

	for _, d := range docs {
		d.bm25 = bm25Score(d, idf, avgLen)
		d.coverage = coverageScore(d, idf, totalIDF)
		d.proximity = proximityScore(d, len(queryTerms))
		if phraseQuery != "" && strings.Contains(d.text, phraseQuery) {
			d.phrase = 1
		}
	}

	// BM25 is unbounded above and the prior's scale is backend-specific: both
	// need min-max normalisation over the pool before they can be weighed
	// against the others.
	normalize(docs, func(d *scoredSection) *float64 { return &d.bm25 })
	priors := make([]float64, len(docs))
	for i, d := range docs {
		priors[i] = d.prior
	}
	normalizeSlice(priors)

	// coverage, proximity and phrase are already in [0,1] by construction and
	// carry an absolute meaning ("all query terms present", "the query appears
	// verbatim"). Min-max normalising them would destroy that: a pool where no
	// candidate covers more than a third of the query would see its best
	// candidate promoted to a perfect 1.
	for i, d := range docs {
		d.score = w.Prior*priors[i] +
			w.BM25*d.bm25 +
			w.Coverage*d.coverage +
			w.Proximity*d.proximity +
			w.Phrase*d.phrase
	}
}

// poolIDF computes each query term's inverse document frequency over the
// candidate pool, using the BM25 probabilistic form clamped at zero so a term
// present in more than half the pool cannot contribute negatively.
func poolIDF(docs []*scoredSection, queryTermCount int) []float64 {
	n := float64(len(docs))
	idf := make([]float64, queryTermCount)

	for slot := range idf {
		var df float64
		for _, d := range docs {
			if len(d.termPositions[slot]) > 0 {
				df++
			}
		}
		idf[slot] = math.Max(0, math.Log(1+(n-df+0.5)/(df+0.5)))
	}

	return idf
}

// bm25Score is Okapi BM25 with pool-level IDF.
func bm25Score(d *scoredSection, idf []float64, avgLen float64) float64 {
	norm := bm25K1 * (1 - bm25B + bm25B*float64(d.tokenCount)/avgLen)

	var score float64
	for slot, positions := range d.termPositions {
		tf := float64(len(positions))
		if tf == 0 {
			continue
		}
		score += idf[slot] * tf * (bm25K1 + 1) / (tf + norm)
	}

	return score
}

// coverageScore is the share of the query's total information content — its
// summed IDF — that the section actually contains. Unlike BM25 it saturates
// immediately: a term present ten times counts exactly as much as once, which is
// what makes it a corrective rather than a duplicate of BM25.
func coverageScore(d *scoredSection, idf []float64, totalIDF float64) float64 {
	if totalIDF <= 0 {
		return 0
	}

	var covered float64
	for slot, positions := range d.termPositions {
		if len(positions) > 0 {
			covered += idf[slot]
		}
	}

	return covered / totalIDF
}

// proximityScore rewards sections where the query terms appear close together.
// It is the ratio of distinct matched terms to the length of the smallest token
// window containing all of them, scaled by the share of the query those terms
// represent — so a tight window covering two terms out of six does not outrank a
// slightly looser one covering all six.
func proximityScore(d *scoredSection, queryTermCount int) float64 {
	distinct := d.matched()

	if distinct == 0 || queryTermCount == 0 {
		return 0
	}
	if distinct == 1 {
		// A single term is always "tight"; without this the window below would
		// be of length 1 and score a perfect 1 for matching one word.
		return 1.0 / float64(queryTermCount)
	}

	type occurrence struct {
		pos  int
		slot int
	}

	var occurrences []occurrence
	for slot, positions := range d.termPositions {
		for _, p := range positions {
			occurrences = append(occurrences, occurrence{pos: p, slot: slot})
		}
	}

	slices.SortFunc(occurrences, func(a, b occurrence) int { return a.pos - b.pos })

	// Smallest window containing all `distinct` terms, by the standard two
	// pointers over the merged, position-sorted occurrences.
	counts := make([]int, queryTermCount)
	covered := 0
	best := math.MaxInt
	left := 0

	for right := range occurrences {
		if counts[occurrences[right].slot] == 0 {
			covered++
		}
		counts[occurrences[right].slot]++

		for covered == distinct {
			if width := occurrences[right].pos - occurrences[left].pos + 1; width < best {
				best = width
			}
			counts[occurrences[left].slot]--
			if counts[occurrences[left].slot] == 0 {
				covered--
			}
			left++
		}
	}

	if best == math.MaxInt || best <= 0 {
		return 0
	}

	tightness := float64(distinct) / float64(best)
	share := float64(distinct) / float64(queryTermCount)

	return tightness * share
}

// normalize min-max normalises one feature across the pool. A pool where every
// candidate scores the same carries no ranking information for that feature, so
// it is flattened to zero rather than to an arbitrary constant that would
// otherwise shift every score by the same amount.
func normalize(docs []*scoredSection, get func(*scoredSection) *float64) {
	values := make([]float64, len(docs))
	for i, d := range docs {
		values[i] = *get(d)
	}

	normalizeSlice(values)

	for i, d := range docs {
		*get(d) = values[i]
	}
}

func normalizeSlice(values []float64) {
	if len(values) == 0 {
		return
	}

	minValue, maxValue := values[0], values[0]
	for _, v := range values[1:] {
		minValue = min(minValue, v)
		maxValue = max(maxValue, v)
	}

	span := maxValue - minValue
	if span <= 0 {
		for i := range values {
			values[i] = 0
		}
		return
	}

	for i := range values {
		values[i] = (values[i] - minValue) / span
	}
}

// rankFromScores turns the scored sections into the rank map applyRanking
// expects, so the reordering and score-rewriting semantics are exactly those of
// the LLM reranker. Ties break on section ID to keep the ordering deterministic
// across runs and instances — the property pagination cursors rely on.
func rankFromScores(docs []*scoredSection) map[model.SectionID]int {
	ordered := make([]*scoredSection, len(docs))
	copy(ordered, docs)

	slices.SortFunc(ordered, func(a, b *scoredSection) int {
		if a.score > b.score {
			return -1
		}
		if a.score < b.score {
			return 1
		}
		return strings.Compare(string(a.id), string(b.id))
	})

	rank := make(map[model.SectionID]int, len(ordered))
	for i, d := range ordered {
		rank[d.id] = i
	}

	return rank
}

// normalizeText lowercases and collapses everything that is not a letter or a
// digit into a single space, so that phrase matching is insensitive to
// punctuation and spacing. It is the string-producing counterpart of
// scanTokens, and must stay tokenisation-compatible with it.
func normalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	space := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			space = false
			continue
		}
		if !space {
			b.WriteByte(' ')
			space = true
		}
	}

	return strings.TrimSpace(b.String())
}

func uniqueTerms(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := make([]string, 0, len(terms))

	for _, t := range terms {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}

	return out
}

// LexicalOption configures a LexicalReranker.
type LexicalOption func(*LexicalReranker)

// WithLexicalWeights overrides the signal weights. Zero-valued fields are taken
// literally — pass DefaultLexicalWeights modified in place to change only some.
func WithLexicalWeights(weights LexicalWeights) LexicalOption {
	return func(r *LexicalReranker) {
		r.weights = weights
	}
}

// WithTokenizer overrides how sections and queries are split into terms.
//
// The default, SimpleTokenizer, covers space-separated scripts; a Chinese or
// Japanese corpus needs a segmenter here. Stemming analyzers were measured and
// rejected — see SimpleTokenizer for the numbers.
//
// The query and the sections must be tokenized the same way, so this replaces
// both at once rather than allowing them to diverge.
func WithTokenizer(tokenizer Tokenizer) LexicalOption {
	return func(r *LexicalReranker) {
		if tokenizer != nil {
			r.tokenizer = tokenizer
		}
	}
}

// NewLexicalReranker builds a model-free reranker over the given section store.
func NewLexicalReranker(store SectionStore, funcs ...LexicalOption) *LexicalReranker {
	reranker := &LexicalReranker{
		store:     store,
		weights:   DefaultLexicalWeights,
		tokenizer: SimpleTokenizer{},
	}

	for _, fn := range funcs {
		fn(reranker)
	}

	return reranker
}
