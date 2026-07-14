// Package eval provides an offline retrieval-quality harness: it runs a golden
// dataset (query → expected relevant sources) through a Retriever and reports
// the standard ranking metrics — Recall@k, Mean Reciprocal Rank (MRR) and
// nDCG@k — so that changes to the retrieval stack (fusion, reranking,
// grounding) can be validated objectively rather than by feel.
//
// The metric functions are pure and dependency-free (this file), so they run in
// the short unit suite. The end-to-end evaluation of a real Codex against a
// dataset lives in the integration tests, gated behind an env var like the
// other Ollama-backed tests.
package eval

import "math"

// DefaultKs are the cut-offs reported by default (Recall@1/3/5/10, nDCG@1/3/5/10).
var DefaultKs = []int{1, 3, 5, 10}

// toSet turns a relevance list into a lookup set, ignoring empty identifiers.
func toSet(relevant []string) map[string]struct{} {
	set := make(map[string]struct{}, len(relevant))
	for _, id := range relevant {
		if id == "" {
			continue
		}
		set[id] = struct{}{}
	}
	return set
}

// RecallAtK is the fraction of the relevant documents that appear within the
// top-k retrieved results: |relevant ∩ top-k| / |relevant|. With a single
// relevant document it degrades to the binary "is the target in the top-k"
// measure. It returns 0 when there are no relevant documents (nothing to find)
// and clamps k to a non-negative value.
func RecallAtK(retrieved, relevant []string, k int) float64 {
	rel := toSet(relevant)
	if len(rel) == 0 {
		return 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}

	found := 0
	for _, id := range retrieved[:max(k, 0)] {
		if _, ok := rel[id]; ok {
			found++
			delete(rel, id) // count each relevant document at most once
		}
	}

	return float64(found) / float64(len(toSet(relevant)))
}

// FirstRelevantRank returns the 1-indexed rank of the first relevant document
// in the retrieved list, or -1 when none of them is relevant.
func FirstRelevantRank(retrieved, relevant []string) int {
	rel := toSet(relevant)
	if len(rel) == 0 {
		return -1
	}
	for i, id := range retrieved {
		if _, ok := rel[id]; ok {
			return i + 1
		}
	}
	return -1
}

// ReciprocalRank is 1/rank of the first relevant document, or 0 when none of
// the relevant documents was retrieved. It is the per-query term averaged into
// the Mean Reciprocal Rank.
func ReciprocalRank(retrieved, relevant []string) float64 {
	rank := FirstRelevantRank(retrieved, relevant)
	if rank < 1 {
		return 0
	}
	return 1 / float64(rank)
}

// PrecisionAtK is the fraction of the top-k retrieved results that are relevant:
// |relevant ∩ top-k| / k'. It is the counterpart of Recall@k on the precision
// axis — where recall asks "did we find the gold documents", precision asks "of
// what we returned, how much is gold". It matters for pipelines that deliberately
// return a small, high-purity evidence set (e.g. grounded iterative retrieval),
// whose value recall@k understates. The denominator k' is min(k, len(retrieved)),
// so a run that returns fewer than k results is not penalised for the empty
// slots. It returns 0 when nothing was retrieved or there are no relevant
// documents.
func PrecisionAtK(retrieved, relevant []string, k int) float64 {
	rel := toSet(relevant)
	if len(rel) == 0 {
		return 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	if k <= 0 {
		return 0
	}

	found := 0
	for _, id := range retrieved[:k] {
		if _, ok := rel[id]; ok {
			found++
			delete(rel, id) // count each relevant document at most once
		}
	}

	return float64(found) / float64(k)
}

// NDCGAtK is the normalised Discounted Cumulative Gain at cut-off k for binary
// relevance: DCG@k / IDCG@k, where each relevant document contributes a gain of
// 1 discounted by log2(position+1). It rewards placing relevant documents
// higher and, unlike Recall@k, is sensitive to their exact rank. It returns 0
// when there are no relevant documents.
func NDCGAtK(retrieved, relevant []string, k int) float64 {
	rel := toSet(relevant)
	numRelevant := len(rel)
	if numRelevant == 0 {
		return 0
	}
	if k > len(retrieved) {
		k = len(retrieved)
	}
	if k < 0 {
		k = 0
	}

	dcg := 0.0
	for i, id := range retrieved[:k] {
		if _, ok := rel[id]; ok {
			dcg += 1 / math.Log2(float64(i+2)) // position i is 0-indexed → rank i+1
			delete(rel, id)                    // each relevant document counts once
		}
	}

	// Ideal DCG: all relevant documents packed at the top (bounded by k).
	ideal := min(numRelevant, k)
	idcg := 0.0
	for i := range ideal {
		idcg += 1 / math.Log2(float64(i+2))
	}
	if idcg == 0 {
		return 0
	}

	return dcg / idcg
}
