package treeindex

import (
	"context"
	"encoding/gob"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/indecis"
)

// TestTreeIndex compares flat and tree-aware rankings of a document's units.
//
//	AMOXTLI_TREEINDEX_SPLITS   comma-separated split dirs (from qasper_markdown.py)
//	AMOXTLI_TREEINDEX_BACKBONE bekko backbone dir, for dense scores (optional)
//	AMOXTLI_TREEINDEX_MODELS   comma-separated name=dir relevance models (optional)
//
// Scores are cached in <split>/scores-<scorer>.gob.
func TestTreeIndex(t *testing.T) {
	splits := os.Getenv("AMOXTLI_TREEINDEX_SPLITS")
	if splits == "" {
		t.Skip("AMOXTLI_TREEINDEX_SPLITS not set")
	}

	for _, dir := range strings.Split(splits, ",") {
		split, err := LoadSplit(dir)
		if err != nil {
			t.Fatal(err)
		}
		reportSplit(t, dir, split)

		scores := map[string]map[string][]float64{}
		scores["bm25:text"], scores["bm25:path"] = bm25Scores(split)

		if backbone := os.Getenv("AMOXTLI_TREEINDEX_BACKBONE"); backbone != "" {
			for _, repr := range []string{"text", "path", "node"} {
				s, err := cached(dir, "dense:"+repr, func() (map[string][]float64, error) {
					return denseScores(backbone, split, repr)
				})
				if err != nil {
					t.Fatal(err)
				}
				scores["dense:"+repr] = s
			}
		}

		var models []string
		if spec := os.Getenv("AMOXTLI_TREEINDEX_MODELS"); spec != "" {
			for _, m := range strings.Split(spec, ",") {
				name, mdir, _ := strings.Cut(m, "=")
				models = append(models, name)
				for _, repr := range []string{"text", "path", "node"} {
					s, err := cached(dir, name+":"+repr, func() (map[string][]float64, error) {
						return modelScores(mdir, split, repr)
					})
					if err != nil {
						t.Fatal(err)
					}
					scores[name+":"+repr] = s
				}
			}
		}

		report(t, filepath.Base(dir), split, scores, models)
	}
}

func reportSplit(t *testing.T, dir string, split *Split) {
	units, gold, depth := 0, 0, 0
	for _, p := range split.Papers {
		units += len(p.Ranked)
		for _, u := range p.Ranked {
			depth = max(depth, u.Depth)
		}
	}
	for _, q := range split.Questions {
		gold += len(q.Gold)
	}
	t.Logf("%s: %d papers, %.1f ranked units/paper, max depth %d, %d questions (%.2f gold units each), %d without located evidence",
		dir, len(split.Papers), float64(units)/float64(len(split.Papers)), depth,
		len(split.Questions), float64(gold)/float64(len(split.Questions)), split.Unmatched)
}

// method is one ranking built from cached scores.
type method struct {
	name string
	rank func(p *Paper, q *Question, s func(string) []float64) []float64
}

func report(t *testing.T, label string, split *Split, scores map[string]map[string][]float64, models []string) {
	get := func(q *Question) func(string) []float64 {
		return func(name string) []float64 { return scores[name][q.ID] }
	}
	hybrid := func(repr string) func(p *Paper, q *Question, s func(string) []float64) []float64 {
		return func(p *Paper, q *Question, s func(string) []float64) []float64 {
			return RRF(s("bm25:"+repr), s("dense:"+repr))
		}
	}
	plain := func(name string) method {
		return method{name, func(p *Paper, q *Question, s func(string) []float64) []float64 { return s(name) }}
	}

	methods := []method{plain("bm25:text"), plain("bm25:path")}
	if _, ok := scores["dense:text"]; ok {
		methods = append(methods,
			plain("dense:text"), plain("dense:path"),
			method{"hybrid:text", hybrid("text")},
			method{"hybrid:path", hybrid("path")},
		)
	}

	for _, m := range models {
		methods = append(methods, plain(m+":text"), plain(m+":path"))
		for _, a := range []float64{0.2, 0.35, 0.5} {
			methods = append(methods, method{fmt.Sprintf("%s:path+anc%.2f", m, a), func(p *Paper, q *Question, s func(string) []float64) []float64 {
				return Ancestors(p, s(m+":path"), s(m+":node"), a)
			}})
		}
		for _, b := range []int{1, 2, 3} {
			methods = append(methods, method{fmt.Sprintf("%s:beam%d", m, b), func(p *Paper, q *Question, s func(string) []float64) []float64 {
				return Beam(p, s(m+":path"), s(m+":node"), hybrid("path")(p, q, s), b)
			}})
		}
		if _, ok := scores["dense:text"]; !ok {
			continue
		}
		for _, repr := range []string{"text", "path"} {
			for _, w := range []float64{0.2, 0.35, 0.5, 0.7} {
				methods = append(methods, method{fmt.Sprintf("hybrid:%s+%s:%s w%.2f", repr, m, repr, w), func(p *Paper, q *Question, s func(string) []float64) []float64 {
					return Blend(hybrid(repr)(p, q, s), probs(s(m+":"+repr)), w)
				}})
			}
		}
		for _, w := range []float64{0.2, 0.35, 0.5} {
			methods = append(methods, method{fmt.Sprintf("dense:text+%s:path w%.2f", m, w), func(p *Paper, q *Question, s func(string) []float64) []float64 {
				return Blend(s("dense:text"), probs(s(m+":path")), w)
			}})
			methods = append(methods, method{fmt.Sprintf("dense:text+%s:text w%.2f", m, w), func(p *Paper, q *Question, s func(string) []float64) []float64 {
				return Blend(s("dense:text"), probs(s(m+":text")), w)
			}})
		}
		for _, w := range []float64{0.35, 0.5, 0.7} {
			methods = append(methods, method{fmt.Sprintf("hybrid:path+%s:path+anc0.35 w%.2f", m, w), func(p *Paper, q *Question, s func(string) []float64) []float64 {
				return Blend(hybrid("path")(p, q, s), probs(Ancestors(p, s(m+":path"), s(m+":node"), 0.35)), w)
			}})
		}
	}

	mrr := map[string][]float64{}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%-44s %6s %6s %6s %6s %6s %7s\n", label, "hit@1", "hit@3", "hit@5", "MRR", "R@5", "nDCG10")
	for _, m := range methods {
		var metrics Metrics
		for _, q := range split.Questions {
			before := metrics.MRR
			metrics.Add(m.rank(split.Papers[q.Paper], q, get(q)), q.Gold)
			mrr[m.name] = append(mrr[m.name], metrics.MRR-before)
		}
		v := metrics.Means()
		fmt.Fprintf(&b, "%-44s %6.3f %6.3f %6.3f %6.3f %6.3f %7.3f\n", m.name, v[0], v[1], v[2], v[3], v[4], v[5])
	}

	// A random ranking, as a floor.
	var floor Metrics
	for _, q := range split.Questions {
		p := split.Papers[q.Paper]
		s := make([]float64, len(p.Ranked))
		for i := range s {
			s[i] = math.Sin(float64(i*7919 + len(q.ID))) // deterministic shuffle
		}
		floor.Add(s, q.Gold)
	}
	v := floor.Means()
	fmt.Fprintf(&b, "%-44s %6.3f %6.3f %6.3f %6.3f %6.3f %7.3f\n", "random", v[0], v[1], v[2], v[3], v[4], v[5])

	// Paired bootstrap of MRR differences for the comparisons that matter.
	fmt.Fprintf(&b, "\nMRR difference (paired bootstrap, 95%% interval)\n")
	for _, c := range [][2]string{
		{"B:path", "B:text"},
		{"hybrid:path+B:path w0.35", "hybrid:path"},
		{"hybrid:path+B:path w0.35", "hybrid:text+B:text w0.35"},
		{"hybrid:path+B:path+anc0.35 w0.35", "hybrid:path+B:path w0.35"},
		{"B:beam1", "B:path"},
		{"dense:text+B:path w0.35", "dense:text"},
		{"dense:text+B:path w0.35", "dense:text+B:text w0.35"},
		{"dense:text", "hybrid:path"},
	} {
		x, y := mrr[c[0]], mrr[c[1]]
		if x == nil || y == nil {
			continue
		}
		d, lo, hi := pairedBootstrap(x, y)
		fmt.Fprintf(&b, "  %-34s - %-30s %+.3f [%+.3f, %+.3f]\n", c[0], c[1], d, lo, hi)
	}
	t.Log(b.String())
}

func pairedBootstrap(x, y []float64) (mean, lo, hi float64) {
	n := len(x)
	diff := make([]float64, n)
	for i := range x {
		diff[i] = x[i] - y[i]
		mean += diff[i]
	}
	mean /= float64(n)
	rng := rand.New(rand.NewPCG(1, 2))
	const rounds = 2000
	means := make([]float64, rounds)
	for r := range means {
		s := 0.0
		for range n {
			s += diff[rng.IntN(n)]
		}
		means[r] = s / float64(n)
	}
	sort.Float64s(means)
	return mean, means[rounds*25/1000], means[rounds*975/1000]
}

// probs turns relevance logits into probabilities (the blend mixes a
// probability with a rank prior).
func probs(logits []float64) []float64 {
	out := make([]float64, len(logits))
	for i, l := range logits {
		out[i] = sigmoid(l)
	}
	return out
}

func bm25Scores(split *Split) (text, path map[string][]float64) {
	build := func(repr func(*Unit) string) map[string][]float64 {
		var docs [][]string
		terms := map[*Unit][]string{}
		for _, p := range split.Papers {
			for _, u := range p.Ranked {
				terms[u] = Terms(repr(u))
				docs = append(docs, terms[u])
			}
		}
		bm := NewBM25(docs)
		out := make(map[string][]float64, len(split.Questions))
		for _, q := range split.Questions {
			query := Terms(q.Question)
			p := split.Papers[q.Paper]
			s := make([]float64, len(p.Ranked))
			for i, u := range p.Ranked {
				s[i] = bm.Score(query, terms[u])
			}
			out[q.ID] = s
		}
		return out
	}
	return build(ReprText), build(ReprPath)
}

// targets returns the units a representation scores and where the score
// goes: ranked units by Rank for text and path, sections with subsections by
// Index for node.
func targets(p *Paper, repr string) ([]*Unit, func(*Unit) int, int, func(*Unit) string) {
	switch repr {
	case "text":
		return p.Ranked, func(u *Unit) int { return u.Rank }, len(p.Ranked), ReprText
	case "path":
		return p.Ranked, func(u *Unit) int { return u.Rank }, len(p.Ranked), ReprPath
	}
	var nodes []*Unit
	for _, u := range p.Units {
		if len(u.Children) > 0 && u != p.Root {
			nodes = append(nodes, u)
		}
	}
	return nodes, func(u *Unit) int { return u.Index }, len(p.Units), ReprNode
}

func denseScores(backbone string, split *Split, repr string) (map[string][]float64, error) {
	m, err := indecis.Open(backbone, indecis.WithMaxLen(512))
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	out := make(map[string][]float64, len(split.Questions))
	byPaper := groupByPaper(split)
	for _, pid := range sortedKeys(byPaper) {
		p := split.Papers[pid]
		units, at, n, text := targets(p, repr)
		texts := make([]string, len(units))
		for i, u := range units {
			texts[i] = text(u)
		}
		uv, err := m.Embed(ctx, texts...)
		if err != nil {
			return nil, err
		}
		for _, q := range byPaper[pid] {
			qv, err := m.Embed(ctx, q.Question)
			if err != nil {
				return nil, err
			}
			s := nanSlice(n)
			for i, u := range units {
				s[at(u)] = dot(qv[0], uv[i])
			}
			out[q.ID] = s
		}
	}
	return out, nil
}

func modelScores(dir string, split *Split, repr string) (map[string][]float64, error) {
	m, err := indecis.Load(dir)
	if err != nil {
		return nil, err
	}
	temperature := m.Temperatures()["pertinent"]
	if temperature <= 0 {
		temperature = 1
	}
	ctx := context.Background()
	out := make(map[string][]float64, len(split.Questions))
	start := time.Now()
	pairs := 0
	for i, q := range split.Questions {
		p := split.Papers[q.Paper]
		units, at, n, text := targets(p, repr)
		inputs := make([]indecis.Input, len(units))
		for j, u := range units {
			inputs[j] = indecis.Input{Context: q.Question, Text: text(u)}
		}
		s := nanSlice(n)
		if len(inputs) > 0 {
			logits, err := m.LogitsInputs(ctx, inputs...)
			if err != nil {
				return nil, err
			}
			for j, u := range units {
				// Calibrated logit: sigmoid of it is the model's P.
				s[at(u)] = logits[j]["pertinent"][0] / temperature
			}
		}
		out[q.ID] = s
		pairs += len(inputs)
		if (i+1)%200 == 0 {
			fmt.Fprintf(os.Stderr, "%s %s: %d/%d questions, %d pairs, %s\n", filepath.Base(dir), repr, i+1, len(split.Questions), pairs, time.Since(start).Round(time.Second))
		}
	}
	return out, nil
}

func cached(dir, name string, compute func() (map[string][]float64, error)) (map[string][]float64, error) {
	path := filepath.Join(dir, "scores-"+strings.ReplaceAll(name, ":", "-")+".gob")
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		var s map[string][]float64
		if err := gob.NewDecoder(f).Decode(&s); err == nil {
			return s, nil
		}
	}
	s, err := compute()
	if err != nil {
		return nil, err
	}
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return s, gob.NewEncoder(f).Encode(s)
}

func groupByPaper(split *Split) map[string][]*Question {
	out := map[string][]*Question{}
	for _, q := range split.Questions {
		out[q.Paper] = append(out[q.Paper], q)
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func nanSlice(n int) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}

func dot(a, b []float32) float64 {
	s := 0.0
	for i := range a {
		s += float64(a[i]) * float64(b[i])
	}
	return s
}
