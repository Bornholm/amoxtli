package retrieval

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

// EvidenceEvaluation is the outcome of an EvidenceEvaluator: the section IDs
// judged relevant to the query, and the grounding (γ) verdict computed over
// those relevant sections.
type EvidenceEvaluation struct {
	// Relevant lists the section IDs to keep (the relevance-filtering signal,
	// formerly produced by the Judge results transformer).
	Relevant []model.SectionID
	// Grounding is the sufficiency verdict over the relevant sections.
	Grounding GroundingResult
}

// EvidenceEvaluator judges, in a single LLM call, both which retrieved sections
// are relevant to the query and whether those relevant sections support a
// reliable, grounded answer. It fuses what used to be two separate LLM passes
// (relevance filtering + grounding) into one.
type EvidenceEvaluator interface {
	Evaluate(ctx context.Context, query string, results []*index.SearchResult) (*EvidenceEvaluation, error)
}

const defaultEvaluatePrompt = `
You are an evidence evaluator for a retrieval-augmented answering system.

## Input
- **Query**: the user's question
- **Documents**: retrieved passages, each with an identifier and content

## Task
1. Select the documents relevant to answering the Query — those that directly
   answer it or provide necessary supporting context. Return their identifiers.
2. Considering **only the selected relevant documents**, decide whether they
   contain enough information to answer the Query using only those documents,
   without any outside knowledge:
   - "valid": they fully support a direct, reliable answer.
   - "partial": they partially support an answer, but key facts are missing.
   - "invalid": they do not support a reliable answer.
3. Give a numeric "score" in [0,1]: your confidence that a reliable, grounded
   answer can be produced from the selected documents alone.

## Output Format (strict JSON, no markdown fencing)
{"relevant": ["id1", "id2"], "status": "valid", "score": 0.0, "explanation": "Brief justification"}

If no documents are relevant:
{"relevant": [], "status": "invalid", "score": 0.0, "explanation": "Brief justification"}
`

// LLMEvidenceEvaluator implements EvidenceEvaluator with a single
// structured-JSON LLM call.
type LLMEvidenceEvaluator struct {
	llm           llm.Client
	store         SectionStore
	maxTotalWords int
}

// NewLLMEvidenceEvaluator builds an EvidenceEvaluator backed by the given LLM
// client. maxTotalWords bounds the evidence budget (defaults to 50000 when <= 0).
func NewLLMEvidenceEvaluator(client llm.Client, store SectionStore, maxTotalWords int) *LLMEvidenceEvaluator {
	return &LLMEvidenceEvaluator{
		llm:           client,
		store:         store,
		maxTotalWords: maxTotalWords,
	}
}

// Evaluate implements EvidenceEvaluator.
func (e *LLMEvidenceEvaluator) Evaluate(ctx context.Context, query string, results []*index.SearchResult) (*EvidenceEvaluation, error) {
	// No evidence at all → nothing relevant, nothing to ground on.
	if len(results) == 0 {
		return &EvidenceEvaluation{
			Relevant: []model.SectionID{},
			Grounding: GroundingResult{
				Status:      GroundingInvalid,
				Score:       0,
				Explanation: "no documents were retrieved",
			},
		}, nil
	}

	userPrompt, err := e.getUserPrompt(ctx, query, results)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	seed, err := text.IntHash(defaultEvaluatePrompt + query)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	ctx = slogx.WithAttrs(ctx, slog.Int("seed", seed))

	completion, err := e.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(
			llm.NewResponseSchema(
				"EvidenceEvaluation",
				"The relevant documents and whether they support a reliable answer to the query",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"relevant": map[string]any{
							"type":        "array",
							"description": "Identifiers of the documents relevant to the query",
							"items":       map[string]any{"type": "string"},
						},
						"status": map[string]any{
							"type":        "string",
							"enum":        []string{"valid", "partial", "invalid"},
							"description": "Whether the relevant documents support a reliable answer",
						},
						"score": map[string]any{
							"type":        "number",
							"description": "Confidence in [0,1] that a grounded answer can be produced",
						},
						"explanation": map[string]any{
							"type":        "string",
							"description": "Brief justification of the verdict",
						},
					},
					"required":             []string{"relevant", "status", "score", "explanation"},
					"additionalProperties": false,
				},
			),
		),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, defaultEvaluatePrompt),
			llm.NewMessage(llm.RoleUser, userPrompt),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	type llmResponse struct {
		Relevant    []string `json:"relevant"`
		Status      string   `json:"status"`
		Score       float64  `json:"score"`
		Explanation string   `json:"explanation"`
	}

	responses, err := llm.ParseJSON[llmResponse](completion.Message())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Missing signal: fail open — keep all evidence (do not silently drop it) and
	// return a neutral verdict so the caller neither abstains nor loops blindly.
	if len(responses) == 0 {
		return &EvidenceEvaluation{
			Relevant: allSectionIDs(results),
			Grounding: GroundingResult{
				Status:      GroundingPartial,
				Score:       0.5,
				Explanation: "evidence evaluator returned no verdict",
			},
		}, nil
	}

	r := responses[0]

	relevant := make([]model.SectionID, 0, len(r.Relevant))
	for _, id := range r.Relevant {
		relevant = append(relevant, model.SectionID(id))
	}

	evaluation := &EvidenceEvaluation{
		Relevant: relevant,
		Grounding: GroundingResult{
			Status:      normalizeGroundingStatus(r.Status),
			Score:       clampScore(r.Score),
			Explanation: r.Explanation,
		},
	}

	slog.DebugContext(ctx, "evidence evaluation",
		slog.Int("relevant", len(evaluation.Relevant)),
		slog.String("status", string(evaluation.Grounding.Status)),
		slog.Float64("score", evaluation.Grounding.Score),
	)

	return evaluation, nil
}

func (e *LLMEvidenceEvaluator) getUserPrompt(ctx context.Context, query string, results []*index.SearchResult) (string, error) {
	sectionsMap, err := e.store.GetSectionsByIDs(ctx, allSectionIDs(results))
	if err != nil {
		return "", errors.WithStack(err)
	}

	maxTotalWords := e.maxTotalWords
	if maxTotalWords <= 0 {
		maxTotalWords = 50000
	}
	totalWords := 0

	var sb strings.Builder
	sb.WriteString("## Query\n\n")
	sb.WriteString(query)
	sb.WriteString("\n\n## Documents\n\n")

	for _, r := range results {
		for _, s := range r.Sections {
			section, exists := sectionsMap[s]
			if !exists {
				continue
			}

			if totalWords >= maxTotalWords {
				break
			}

			content, err := section.Content()
			if err != nil {
				return "", errors.WithStack(err)
			}

			sb.WriteString("### Document ")
			sb.WriteString(string(section.ID()))
			sb.WriteString("\n\n")

			// Give the identifier explicitly so the model returns the bare ID in
			// "relevant" (not the "Document <id>" heading).
			sb.WriteString("**Identifier:** ")
			sb.WriteString(string(section.ID()))
			sb.WriteString("\n\n")

			words := strings.Fields(string(content))
			remaining := maxTotalWords - totalWords
			if len(words) > remaining {
				words = words[:remaining]
			}
			totalWords += len(words)

			sb.WriteString(strings.Join(words, " "))
			sb.WriteString("\n\n")
		}
	}

	return sb.String(), nil
}

// allSectionIDs collects every section ID referenced by the results.
func allSectionIDs(results []*index.SearchResult) []model.SectionID {
	var ids []model.SectionID
	for _, r := range results {
		ids = append(ids, r.Sections...)
	}
	return ids
}

// FilterRelevant keeps only the sections whose ID is in relevant, dropping
// results left with no sections. Input slices are not mutated. It is the
// relevance-filtering step applied from an EvidenceEvaluation.
func FilterRelevant(results []*index.SearchResult, relevant []model.SectionID) []*index.SearchResult {
	keep := make(map[model.SectionID]struct{}, len(relevant))
	for _, id := range relevant {
		keep[id] = struct{}{}
	}

	out := make([]*index.SearchResult, 0, len(results))
	for _, r := range results {
		sections := make([]model.SectionID, 0, len(r.Sections))
		for _, s := range r.Sections {
			if _, ok := keep[s]; ok {
				sections = append(sections, s)
			}
		}
		if len(sections) == 0 {
			continue
		}
		out = append(out, &index.SearchResult{
			Source:        r.Source,
			Sections:      sections,
			Score:         r.Score,
			SectionScores: filterSectionScores(r.SectionScores, sections),
		})
	}

	return out
}

// filterSectionScores returns the subset of scores whose section is in keep,
// preserving the per-section scores of a SearchResult after its section set has
// been filtered. It returns nil when scores is nil.
func filterSectionScores(scores map[model.SectionID]float64, keep []model.SectionID) map[model.SectionID]float64 {
	if scores == nil {
		return nil
	}
	out := make(map[model.SectionID]float64, len(keep))
	for _, s := range keep {
		if score, ok := scores[s]; ok {
			out[s] = score
		}
	}
	return out
}

var _ EvidenceEvaluator = &LLMEvidenceEvaluator{}
