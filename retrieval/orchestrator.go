package retrieval

import (
	"context"
	"log/slog"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// SearchFunc performs a single retrieval step. It is the seam through which the
// Orchestrator reaches the underlying index, keeping the orchestration logic
// decoupled from amoxtli's Codex/ingest layer (and unit-testable with a stub).
type SearchFunc func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error)

// Result is the outcome of Orchestrator.Search: the final fused (and, when an
// evaluator is configured, relevance-filtered) evidence, the grounding verdict
// (nil when no evaluator is configured) and the number of extra re-retrieval
// rounds performed. Unlike Corpus' AskResult it carries no answer: amoxtli does
// not generate one — the caller decides what to do with the evidence and the
// verdict (e.g. abstain when Grounding is invalid/low).
type Result struct {
	Results   []*index.SearchResult
	Grounding *GroundingResult
	Rounds    int
}

// Orchestrator unifies retrieval with the MothRAG-derived mechanisms on top of a
// plain SearchFunc:
//
//   - query decomposition: when a QueryDecomposer is configured, the original
//     query and its sub-questions are searched and their evidence fused;
//   - evidence evaluation: when an EvidenceEvaluator is configured, the fused
//     evidence is relevance-filtered and a grounding verdict is produced in a
//     single LLM call;
//   - iterative re-retrieval: when a QueryReformulator is configured and the
//     verdict is not confident, the query is reformulated and searched again
//     (up to MaxRounds), enlarging the evidence set;
//
// With none of these configured it degrades to a single SearchFunc call.
type Orchestrator struct {
	search       SearchFunc
	evaluator    EvidenceEvaluator
	reformulator QueryReformulator
	decomposer   QueryDecomposer
	minScore     float64
	maxRounds    int
}

// OrchestratorOption configures an Orchestrator.
type OrchestratorOption func(*Orchestrator)

// WithEvidenceEvaluator enables the fused relevance-filtering + grounding (γ)
// evaluator: after retrieval, the evaluator keeps the relevant evidence and
// judges whether it supports a reliable answer. The verdict is returned in
// Result.Grounding and gates iterative re-retrieval.
func WithEvidenceEvaluator(evaluator EvidenceEvaluator) OrchestratorOption {
	return func(o *Orchestrator) {
		o.evaluator = evaluator
	}
}

// WithGroundingMinScore sets the grounding score threshold below which the
// verdict is considered not confident (default 0.4). Only meaningful together
// with WithEvidenceEvaluator.
func WithGroundingMinScore(minScore float64) OrchestratorOption {
	return func(o *Orchestrator) {
		o.minScore = minScore
	}
}

// WithQueryReformulator enables grounding-driven re-retrieval: when the evidence
// is not confidently grounded the query is reformulated and searched again.
// Requires WithEvidenceEvaluator.
func WithQueryReformulator(reformulator QueryReformulator) OrchestratorOption {
	return func(o *Orchestrator) {
		o.reformulator = reformulator
	}
}

// WithMaxRounds caps the number of extra re-retrieval rounds (default 1).
func WithMaxRounds(rounds int) OrchestratorOption {
	return func(o *Orchestrator) {
		if rounds > 0 {
			o.maxRounds = rounds
		}
	}
}

// WithQueryDecomposer enables query decomposition: a complex question is split
// into sub-questions, each searched independently and their evidence fused
// before verification.
func WithQueryDecomposer(decomposer QueryDecomposer) OrchestratorOption {
	return func(o *Orchestrator) {
		o.decomposer = decomposer
	}
}

// NewOrchestrator builds an Orchestrator over the given SearchFunc.
func NewOrchestrator(search SearchFunc, funcs ...OrchestratorOption) *Orchestrator {
	o := &Orchestrator{
		search:    search,
		minScore:  0.4,
		maxRounds: 1,
	}
	for _, fn := range funcs {
		fn(o)
	}
	return o
}

// Search runs the orchestrated retrieval for the given query.
func (o *Orchestrator) Search(ctx context.Context, query string, maxResults int, collections []model.CollectionID) (*Result, error) {
	// Round 0: (optionally decomposed) retrieval.
	results, err := o.retrieve(ctx, query, maxResults, collections)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	result := &Result{}

	// Evidence evaluation (relevance filtering + grounding verdict) gating
	// iterative re-retrieval.
	var grounding *GroundingResult
	rounds := 0
	if o.evaluator != nil {
		for len(results) > 0 {
			evaluation, err := o.evaluator.Evaluate(ctx, query, results)
			if err != nil {
				return nil, errors.WithStack(err)
			}

			// Keep only the relevant evidence, then gate on its verdict. Note we
			// do not stop when filtering empties the evidence: an empty, not
			// confident verdict is exactly when re-retrieval is most useful.
			results = FilterRelevant(results, evaluation.Relevant)
			verdict := evaluation.Grounding
			grounding = &verdict

			confident := verdict.Status == GroundingValid && verdict.Score >= o.minScore
			if confident || o.reformulator == nil || rounds >= o.maxRounds {
				break
			}

			reformulated, err := o.reformulator.Reformulate(ctx, query, grounding.Explanation)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			if reformulated == "" || reformulated == query {
				break
			}

			rounds++
			slog.InfoContext(ctx, "iterative re-retrieval",
				slog.String("reformulated_query", reformulated),
				slog.Int("round", rounds),
			)

			more, err := o.retrieve(ctx, reformulated, maxResults, collections)
			if err != nil {
				return nil, errors.WithStack(err)
			}

			results = fuseResults(results, more)
		}
	}

	result.Grounding = grounding
	result.Rounds = rounds
	result.Results = results

	return result, nil
}

// retrieve performs a single retrieval step. When a QueryDecomposer is
// configured, it searches the original query plus each sub-question and fuses
// the evidence; otherwise it is a plain SearchFunc call.
func (o *Orchestrator) retrieve(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
	if o.decomposer == nil {
		results, err := o.search(ctx, query, maxResults, collections)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return results, nil
	}

	subQueries, err := o.decomposer.Decompose(ctx, query)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Always include the original query, then each distinct sub-question.
	queries := make([]string, 0, len(subQueries)+1)
	queries = append(queries, query)
	for _, sq := range subQueries {
		if sq == query {
			continue
		}
		queries = append(queries, sq)
	}

	fused := make([]*index.SearchResult, 0)
	for _, q := range queries {
		r, err := o.search(ctx, q, maxResults, collections)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		fused = fuseResults(fused, r)
	}

	return fused, nil
}

// fuseResults unions several result groups, de-duplicating at the section level
// (a section already contributed by an earlier group is dropped) and discarding
// results left with no sections. Input slices are not mutated.
func fuseResults(groups ...[]*index.SearchResult) []*index.SearchResult {
	seen := map[model.SectionID]struct{}{}
	out := make([]*index.SearchResult, 0)

	for _, group := range groups {
		for _, r := range group {
			kept := make([]model.SectionID, 0, len(r.Sections))
			for _, sectionID := range r.Sections {
				if _, exists := seen[sectionID]; exists {
					continue
				}
				seen[sectionID] = struct{}{}
				kept = append(kept, sectionID)
			}

			if len(kept) == 0 {
				continue
			}

			out = append(out, &index.SearchResult{
				Source:        r.Source,
				Sections:      kept,
				Score:         r.Score,
				SectionScores: filterSectionScores(r.SectionScores, kept),
			})
		}
	}

	return out
}
