package eval

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/pkg/errors"
)

// Retriever is the minimal contract the harness evaluates: given a query and a
// cut-off k, return up to k document source identifiers ranked best-first. It
// is deliberately decoupled from *Codex so the harness can score any retrieval
// implementation (a Codex, a single index, a baseline BM25, a mock).
type Retriever interface {
	Retrieve(ctx context.Context, query string, k int) ([]string, error)
}

// RetrieverFunc adapts a function to the Retriever interface.
type RetrieverFunc func(ctx context.Context, query string, k int) ([]string, error)

// Retrieve implements Retriever.
func (fn RetrieverFunc) Retrieve(ctx context.Context, query string, k int) ([]string, error) {
	return fn(ctx, query, k)
}

// FromSearchResults adapts a search function returning ranked
// index.SearchResult (such as Codex.Search) into a Retriever, using each
// result's Source as the identifier. Results with a nil Source are skipped.
func FromSearchResults(search func(ctx context.Context, query string, k int) ([]*index.SearchResult, error)) Retriever {
	return RetrieverFunc(func(ctx context.Context, query string, k int) ([]string, error) {
		results, err := search(ctx, query, k)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		ids := make([]string, 0, len(results))
		for _, r := range results {
			if r == nil || r.Source == nil {
				continue
			}
			ids = append(ids, r.Source.String())
		}
		return ids, nil
	})
}

// QueryReport holds the metrics computed for a single query.
type QueryReport struct {
	QueryID string
	Query   string
	Lang    string
	Tags    []string
	// TargetRank is the 1-indexed rank of the first relevant document, or -1 if
	// none was retrieved.
	TargetRank     int
	ReciprocalRank float64
	RecallAtK      map[int]float64
	NDCGAtK        map[int]float64
}

// Report aggregates the metrics over a whole dataset run.
type Report struct {
	Dataset    string
	NumQueries int
	Ks         []int
	// MRR is the mean reciprocal rank across all queries.
	MRR float64
	// RecallAtK and NDCGAtK are the dataset-averaged metrics per cut-off.
	RecallAtK map[int]float64
	NDCGAtK   map[int]float64
	PerQuery  []QueryReport
}

// Evaluate runs every query in the dataset through the retriever and returns the
// aggregated report. It fetches the largest requested cut-off worth of results
// per query. ks defaults to DefaultKs when empty. A retriever error aborts the
// run (a query with no results is not an error — it simply scores 0).
func Evaluate(ctx context.Context, ds *Dataset, r Retriever, ks ...int) (*Report, error) {
	if ds == nil {
		return nil, errors.New("eval: nil dataset")
	}
	if len(ks) == 0 {
		ks = DefaultKs
	}
	ks = normalizeKs(ks)
	maxK := ks[len(ks)-1]

	perQuery := make([]QueryReport, 0, len(ds.Queries))
	for _, q := range ds.Queries {
		retrieved, err := r.Retrieve(ctx, q.Query, maxK)
		if err != nil {
			return nil, errors.Wrapf(err, "eval: retrieving query %q", queryLabel(q))
		}

		qr := QueryReport{
			QueryID:        q.ID,
			Query:          q.Query,
			Lang:           q.Lang,
			Tags:           q.Tags,
			TargetRank:     FirstRelevantRank(retrieved, q.RelevantSources),
			ReciprocalRank: ReciprocalRank(retrieved, q.RelevantSources),
			RecallAtK:      make(map[int]float64, len(ks)),
			NDCGAtK:        make(map[int]float64, len(ks)),
		}
		for _, k := range ks {
			qr.RecallAtK[k] = RecallAtK(retrieved, q.RelevantSources, k)
			qr.NDCGAtK[k] = NDCGAtK(retrieved, q.RelevantSources, k)
		}

		perQuery = append(perQuery, qr)
	}

	return aggregate(ds.Name, ks, perQuery), nil
}

// aggregate averages the per-query metrics into a Report. It is shared by
// Evaluate and the segmentation helpers so every (sub-)report is computed the
// same way.
func aggregate(name string, ks []int, perQuery []QueryReport) *Report {
	report := &Report{
		Dataset:    name,
		NumQueries: len(perQuery),
		Ks:         ks,
		RecallAtK:  make(map[int]float64, len(ks)),
		NDCGAtK:    make(map[int]float64, len(ks)),
		PerQuery:   perQuery,
	}
	if len(perQuery) == 0 {
		return report
	}

	var sumRR float64
	sumRecall := make(map[int]float64, len(ks))
	sumNDCG := make(map[int]float64, len(ks))
	for _, qr := range perQuery {
		sumRR += qr.ReciprocalRank
		for _, k := range ks {
			sumRecall[k] += qr.RecallAtK[k]
			sumNDCG[k] += qr.NDCGAtK[k]
		}
	}

	n := float64(len(perQuery))
	report.MRR = sumRR / n
	for _, k := range ks {
		report.RecallAtK[k] = sumRecall[k] / n
		report.NDCGAtK[k] = sumNDCG[k] / n
	}

	return report
}

// BySegment partitions the report's queries by the given key function and
// re-aggregates each partition into its own sub-report. Segments with an empty
// key are grouped under "" and can be ignored by the caller. It is the basis
// for query-type analysis (by language, intent, complexity, ...).
func (r *Report) BySegment(key func(QueryReport) string) map[string]*Report {
	groups := make(map[string][]QueryReport)
	for _, qr := range r.PerQuery {
		k := key(qr)
		groups[k] = append(groups[k], qr)
	}

	out := make(map[string]*Report, len(groups))
	for seg, qrs := range groups {
		name := r.Dataset
		if seg != "" {
			name = r.Dataset + " [" + seg + "]"
		}
		out[seg] = aggregate(name, r.Ks, qrs)
	}
	return out
}

// ByLang segments the report by query language.
func (r *Report) ByLang() map[string]*Report {
	return r.BySegment(func(qr QueryReport) string { return qr.Lang })
}

// normalizeKs returns the cut-offs sorted ascending, de-duplicated and stripped
// of non-positive values.
func normalizeKs(ks []int) []int {
	seen := make(map[int]struct{}, len(ks))
	out := make([]int, 0, len(ks))
	for _, k := range ks {
		if k < 1 {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		out = append(out, 1)
	}
	sort.Ints(out)
	return out
}

func queryLabel(q Query) string {
	if q.ID != "" {
		return q.ID
	}
	return q.Query
}

// String renders the aggregate report as a compact, human-readable table,
// suitable for logging in a manual evaluation run.
func (r *Report) String() string {
	var b strings.Builder
	name := r.Dataset
	if name == "" {
		name = "dataset"
	}
	fmt.Fprintf(&b, "eval report — %s (%d queries)\n", name, r.NumQueries)
	fmt.Fprintf(&b, "  MRR: %.3f\n", r.MRR)
	for _, k := range r.Ks {
		fmt.Fprintf(&b, "  Recall@%-2d: %.3f   nDCG@%-2d: %.3f\n", k, r.RecallAtK[k], k, r.NDCGAtK[k])
	}
	return b.String()
}
