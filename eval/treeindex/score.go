package treeindex

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// MaxWords bounds how many words of a unit a scorer reads, like the
// relevance model stages of amoxtli (retrieval.DefaultMaxSectionWords).
const MaxWords = 200

// ReprText is the unit as amoxtli indexes it: its own text, heading
// included; a split continuing a section has no heading.
func ReprText(u *Unit) string {
	return Truncate(u.Text, MaxWords)
}

// ReprPath prefixes the unit's body with the headings leading to it, so a
// split continuing a section, or a subsection, carries where it sits.
func ReprPath(u *Unit) string {
	body := Truncate(u.Body, MaxWords)
	if len(u.Path) == 0 {
		return body
	}
	return strings.Join(u.Path, " > ") + "\n\n" + body
}

// ReprNode describes a section with its subsections, as a tree navigator
// sees it: the path, the subsection headings, and the start of the whole
// section's text (an extractive stand-in for PageIndex's node summaries).
func ReprNode(u *Unit) string {
	var b strings.Builder
	b.WriteString(strings.Join(u.Path, " > "))
	var titles []string
	for _, c := range u.Children {
		if c.Title != "" {
			titles = append(titles, c.Title)
		}
	}
	if len(titles) > 0 {
		b.WriteString("\nSubsections: " + strings.Join(titles, "; "))
	}
	b.WriteString("\n\n" + Truncate(subtreeText(u), MaxWords))
	return b.String()
}

func subtreeText(u *Unit) string {
	parts := []string{u.Body}
	for _, c := range u.Children {
		parts = append(parts, subtreeText(c))
	}
	return strings.Join(parts, " ")
}

var stopwords = func() map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(`a an the of to in on for and or is are was were be been by with as at from
		that this these those it its their they them we our what which who how do does did than then there
		not no but if into about over such can could would should may might also use used using`) {
		m[w] = true
	}
	return m
}()

// Terms are the BM25 terms of a text.
func Terms(s string) []string {
	words := Words(s)
	out := words[:0]
	for _, w := range words {
		if !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// BM25 scores texts against queries with statistics from a fixed corpus.
type BM25 struct {
	df    map[string]int
	n     int
	avgdl float64
}

// NewBM25 computes the document frequencies of a corpus.
func NewBM25(docs [][]string) *BM25 {
	b := &BM25{df: make(map[string]int), n: len(docs)}
	total := 0
	for _, d := range docs {
		total += len(d)
		seen := make(map[string]bool)
		for _, t := range d {
			if !seen[t] {
				seen[t] = true
				b.df[t]++
			}
		}
	}
	if b.n > 0 {
		b.avgdl = float64(total) / float64(b.n)
	}
	return b
}

// Score returns the BM25 score of a document for a query.
func (b *BM25) Score(query, doc []string) float64 {
	const k1, bb = 1.2, 0.75
	tf := make(map[string]int, len(doc))
	for _, t := range doc {
		tf[t]++
	}
	score := 0.0
	seen := make(map[string]bool)
	for _, t := range query {
		if seen[t] {
			continue
		}
		seen[t] = true
		f := float64(tf[t])
		if f == 0 {
			continue
		}
		idf := math.Log(1 + (float64(b.n)-float64(b.df[t])+0.5)/(float64(b.df[t])+0.5))
		score += idf * f * (k1 + 1) / (f + k1*(1-bb+bb*float64(len(doc))/b.avgdl))
	}
	return score
}

// Order returns the indices of scores, best first (stable on ties).
func Order(scores []float64) []int {
	order := make([]int, len(scores))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(scores[b], scores[a])
	})
	return order
}

// FromOrder turns a ranking into scores decreasing with the position.
func FromOrder(order []int) []float64 {
	scores := make([]float64, len(order))
	for pos, i := range order {
		scores[i] = float64(len(order) - pos)
	}
	return scores
}

// RRF fuses rankings by reciprocal rank (k = 60).
func RRF(lists ...[]float64) []float64 {
	out := make([]float64, len(lists[0]))
	for _, l := range lists {
		for pos, i := range Order(l) {
			out[i] += 1 / (60 + float64(pos) + 1)
		}
	}
	return out
}

// Blend mixes a probability with the position in a prior ranking, as
// amoxtli's ModelReranker does: weight*p + (1-weight)*prior, the prior going
// from 1 (first) to 0 (last).
func Blend(prior, p []float64, weight float64) []float64 {
	out := make([]float64, len(p))
	order := Order(prior)
	for pos, i := range order {
		r := 1.0
		if len(order) > 1 {
			r = 1 - float64(pos)/float64(len(order)-1)
		}
		out[i] = weight*p[i] + (1-weight)*r
	}
	return out
}

// Ancestors mixes each unit's score with the best score of the sections
// containing it: (1-a)*unit + a*max(ancestors). node is indexed by
// Unit.Index and holds the score of the sections with subsections. A
// top-level unit is its own ancestor.
func Ancestors(p *Paper, unit, node []float64, a float64) []float64 {
	out := make([]float64, len(unit))
	for _, u := range p.Ranked {
		best := math.Inf(-1)
		for anc := u.Parent; anc != nil && anc != p.Root; anc = anc.Parent {
			best = max(best, node[anc.Index])
		}
		if math.IsInf(best, -1) {
			best = unit[u.Rank]
		}
		out[u.Rank] = (1-a)*unit[u.Rank] + a*best
	}
	return out
}

// Beam descends the tree like a navigator committing to branches: at each
// level, the beam best sections or units of the frontier (scored with node
// for sections with subsections, unit otherwise) are kept and the
// subsections of the kept sections form the next frontier. The units
// reached come first, ordered by unit; the others follow in fallback order.
func Beam(p *Paper, unit, node, fallback []float64, beam int) []float64 {
	score := func(u *Unit) float64 {
		if len(u.Children) > 0 {
			return node[u.Index]
		}
		if u.Rank < 0 {
			return math.Inf(-1)
		}
		return unit[u.Rank]
	}

	reached := make(map[int]bool)
	frontier := p.Root.Children
	for len(frontier) > 0 {
		slices.SortStableFunc(frontier, func(a, b *Unit) int { return cmp.Compare(score(b), score(a)) })
		var next []*Unit
		for _, u := range frontier[:min(beam, len(frontier))] {
			if u.Rank >= 0 {
				reached[u.Rank] = true
			}
			next = append(next, u.Children...)
		}
		frontier = next
	}

	out := make([]float64, len(unit))
	base := FromOrder(Order(fallback))
	for i := range out {
		out[i] = base[i]
		if reached[i] {
			// Reached units rank above every other one, by their own score.
			out[i] = 1e6 + 1e3*sigmoid(unit[i])
		}
	}
	return out
}

func sigmoid(x float64) float64 { return 1 / (1 + math.Exp(-x)) }

// Metrics accumulates ranking metrics over questions.
type Metrics struct {
	N                         int
	Hit1, Hit3, Hit5, MRR, R5 float64
	NDCG10                    float64
}

// Add scores one ranking against its gold units.
func (m *Metrics) Add(scores []float64, gold map[int]bool) {
	order := Order(scores)
	m.N++
	first := -1
	found5 := 0
	dcg, idcg := 0.0, 0.0
	for pos, i := range order {
		if !gold[i] {
			continue
		}
		if first < 0 {
			first = pos
		}
		if pos < 5 {
			found5++
		}
		if pos < 10 {
			dcg += 1 / math.Log2(float64(pos)+2)
		}
	}
	for k := 0; k < min(len(gold), 10); k++ {
		idcg += 1 / math.Log2(float64(k)+2)
	}
	if first >= 0 {
		m.MRR += 1 / float64(first+1)
		if first < 1 {
			m.Hit1++
		}
		if first < 3 {
			m.Hit3++
		}
		if first < 5 {
			m.Hit5++
		}
	}
	m.R5 += float64(found5) / float64(len(gold))
	if idcg > 0 {
		m.NDCG10 += dcg / idcg
	}
}

// Means returns hit@1, hit@3, hit@5, MRR, recall@5 and nDCG@10.
func (m *Metrics) Means() [6]float64 {
	n := float64(max(m.N, 1))
	return [6]float64{m.Hit1 / n, m.Hit3 / n, m.Hit5 / n, m.MRR / n, m.R5 / n, m.NDCG10 / n}
}
