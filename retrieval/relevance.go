package retrieval

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

// PassageScore is a relevance model's verdict on a passage (or on several
// passages joined) for a query. Each field is a probability.
type PassageScore struct {
	// Relevant is the probability that the passage answers the query or brings
	// context needed to answer it.
	Relevant float64
	// Complete is the probability that the passage is enough to answer the
	// query without outside knowledge.
	Complete float64
	// Partial is the probability that the passage supports part of an answer,
	// with key facts missing.
	Partial float64
}

// RelevanceModel scores passages against a query without generating text,
// typically a small local cross-encoder. It is the seam through which the
// evidence evaluator and the reranker run without an LLM.
type RelevanceModel interface {
	Score(ctx context.Context, query string, passages []string) ([]PassageScore, error)
}

// Defaults of the relevance model stages.
const (
	// DefaultRelevanceThreshold is the probability above which a section is
	// considered relevant.
	DefaultRelevanceThreshold = 0.5
	// DefaultSufficiencyPassages is how many of the most relevant sections are
	// joined to judge whether the evidence is enough.
	DefaultSufficiencyPassages = 3
)

// ModelEvidenceEvaluator implements EvidenceEvaluator with a RelevanceModel:
// each section is scored against the query, the relevant ones are kept, and
// the most relevant ones, joined, are scored again to produce the grounding
// verdict.
type ModelEvidenceEvaluator struct {
	model           RelevanceModel
	store           SectionStore
	maxSectionWords int
	threshold       float64
	passages        int
}

// ModelEvaluatorOption configures a ModelEvidenceEvaluator.
type ModelEvaluatorOption func(*ModelEvidenceEvaluator)

// WithRelevanceThreshold sets the probability above which a section is kept
// (default DefaultRelevanceThreshold).
func WithRelevanceThreshold(threshold float64) ModelEvaluatorOption {
	return func(e *ModelEvidenceEvaluator) {
		e.threshold = threshold
	}
}

// WithSufficiencyPassages sets how many of the most relevant sections are
// joined to judge sufficiency (default DefaultSufficiencyPassages).
func WithSufficiencyPassages(n int) ModelEvaluatorOption {
	return func(e *ModelEvidenceEvaluator) {
		if n > 0 {
			e.passages = n
		}
	}
}

// WithModelMaxSectionWords bounds how many words of each section the model
// reads (default DefaultMaxSectionWords).
func WithModelMaxSectionWords(n int) ModelEvaluatorOption {
	return func(e *ModelEvidenceEvaluator) {
		if n > 0 {
			e.maxSectionWords = n
		}
	}
}

// NewModelEvidenceEvaluator builds an EvidenceEvaluator backed by a
// RelevanceModel.
func NewModelEvidenceEvaluator(m RelevanceModel, store SectionStore, funcs ...ModelEvaluatorOption) *ModelEvidenceEvaluator {
	e := &ModelEvidenceEvaluator{
		model:           m,
		store:           store,
		maxSectionWords: DefaultMaxSectionWords,
		threshold:       DefaultRelevanceThreshold,
		passages:        DefaultSufficiencyPassages,
	}
	for _, fn := range funcs {
		fn(e)
	}
	return e
}

// Evaluate implements EvidenceEvaluator.
func (e *ModelEvidenceEvaluator) Evaluate(ctx context.Context, query string, results []*index.SearchResult) (*EvidenceEvaluation, error) {
	evaluation := &EvidenceEvaluation{
		Relevant: []model.SectionID{},
		Grounding: GroundingResult{
			Status:      GroundingInvalid,
			Explanation: "no documents were retrieved",
		},
	}

	ids, texts, err := loadSectionTexts(ctx, e.store, results, e.maxSectionWords)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if len(ids) == 0 {
		return evaluation, nil
	}

	scores, err := e.model.Score(ctx, query, texts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	relevant := make([]int, 0, len(ids))
	for i, s := range scores {
		if s.Relevant >= e.threshold {
			relevant = append(relevant, i)
		}
	}
	slices.SortStableFunc(relevant, func(a, b int) int {
		return cmp.Compare(scores[b].Relevant, scores[a].Relevant)
	})
	for _, i := range relevant {
		evaluation.Relevant = append(evaluation.Relevant, ids[i])
	}

	if len(relevant) == 0 {
		evaluation.Grounding.Explanation = fmt.Sprintf("none of the %d retrieved passages is relevant to the query", len(ids))
		return evaluation, nil
	}

	joined := make([]string, 0, e.passages)
	for _, i := range relevant[:min(e.passages, len(relevant))] {
		joined = append(joined, texts[i])
	}
	verdict, err := e.model.Score(ctx, query, []string{strings.Join(joined, "\n\n")})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	v := verdict[0]

	status := GroundingInvalid
	switch {
	case v.Complete >= v.Partial && v.Complete >= 1-v.Complete-v.Partial:
		status = GroundingValid
	case v.Partial >= 1-v.Complete-v.Partial:
		status = GroundingPartial
	}
	evaluation.Grounding = GroundingResult{
		Status: status,
		Score:  clampScore(v.Complete),
		Explanation: fmt.Sprintf("%d of %d retrieved passages are relevant; the most relevant ones answer the query completely with probability %.2f, partially with probability %.2f",
			len(relevant), len(ids), v.Complete, v.Partial),
	}

	slog.DebugContext(ctx, "evidence evaluation",
		slog.Int("relevant", len(evaluation.Relevant)),
		slog.String("status", string(evaluation.Grounding.Status)),
		slog.Float64("score", evaluation.Grounding.Score),
	)

	return evaluation, nil
}

// DefaultRerankWeight is the share of the relevance model in the reranking
// score, the rest coming from the order of the fused results.
const DefaultRerankWeight = 0.2

// ModelReranker implements ingest.Reranker with a RelevanceModel. Each section
// is scored by blending its probability of being relevant with its position in
// the fused results: the model refines the fusion order rather than replacing
// it. Results are ordered by their best section.
type ModelReranker struct {
	model           RelevanceModel
	store           SectionStore
	maxSectionWords int
	weight          float64
}

// ModelRerankerOption configures a ModelReranker.
type ModelRerankerOption func(*ModelReranker)

// WithRerankWeight sets the share of the relevance model in the reranking
// score, between 0 (fusion order only) and 1 (model only). Default
// DefaultRerankWeight.
func WithRerankWeight(weight float64) ModelRerankerOption {
	return func(r *ModelReranker) {
		r.weight = min(max(weight, 0), 1)
	}
}

// WithRerankMaxSectionWords bounds how many words of each section the model
// reads (default DefaultMaxSectionWords).
func WithRerankMaxSectionWords(n int) ModelRerankerOption {
	return func(r *ModelReranker) {
		if n > 0 {
			r.maxSectionWords = n
		}
	}
}

// NewModelReranker builds a reranker backed by a RelevanceModel.
func NewModelReranker(m RelevanceModel, store SectionStore, funcs ...ModelRerankerOption) *ModelReranker {
	r := &ModelReranker{model: m, store: store, maxSectionWords: DefaultMaxSectionWords, weight: DefaultRerankWeight}
	for _, fn := range funcs {
		fn(r)
	}
	return r
}

// Rerank implements ingest.Reranker.
func (r *ModelReranker) Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error) {
	if len(results) <= 1 {
		return results, nil
	}

	ids, texts, err := loadSectionTexts(ctx, r.store, results, r.maxSectionWords)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	scores, err := r.model.Score(ctx, query, texts)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Sections come in fusion order: the first one gets a prior of 1, the
	// last one 0.
	blended := make([]float64, len(ids))
	for i, s := range scores {
		prior := 1.0
		if len(ids) > 1 {
			prior = 1 - float64(i)/float64(len(ids)-1)
		}
		blended[i] = r.weight*s.Relevant + (1-r.weight)*prior
	}

	order := make([]int, len(ids))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(blended[b], blended[a])
	})

	rank := make(map[model.SectionID]int, len(order))
	for pos, i := range order {
		if _, seen := rank[ids[i]]; !seen {
			rank[ids[i]] = pos
		}
	}

	return applyRanking(results, rank), nil
}

// loadSectionTexts returns the identifier and the first words of every section
// of the results, in result order. Sections missing from the store are
// skipped.
func loadSectionTexts(ctx context.Context, store SectionStore, results []*index.SearchResult, maxSectionWords int) ([]model.SectionID, []string, error) {
	sectionsMap, err := store.GetSectionsByIDs(ctx, allSectionIDs(results))
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	var (
		ids   []model.SectionID
		texts []string
	)
	for _, res := range results {
		for _, s := range res.Sections {
			section, exists := sectionsMap[s]
			if !exists {
				continue
			}
			content, err := section.Content()
			if err != nil {
				return nil, nil, errors.WithStack(err)
			}
			text, _ := truncateSection(string(content), maxSectionWords, maxSectionWords)
			ids = append(ids, s)
			texts = append(texts, text)
		}
	}

	return ids, texts, nil
}

var _ EvidenceEvaluator = &ModelEvidenceEvaluator{}
