package retrieval

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/prompt"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

// LLMReranker reorders search results by asking an LLM to rank the retrieved
// sections by relevance to the query. Unlike the Judge (which filters), the
// reranker only reorders: every input result is preserved, but its position and
// score reflect the LLM-assessed relevance. It reuses the same maxTotalWords
// budget as the other LLM retrieval components to bound the prompt size.
type LLMReranker struct {
	llm             llm.Client
	store           SectionStore
	maxTotalWords   int
	maxSectionWords int
}

const defaultRerankerPrompt = `
You are a document retrieval reranker.

## Input
- **Query**: The user's search query
- **Documents**: A list of documents, each with an identifier and content

## Task
Order ALL the provided document identifiers from the most relevant to the query
to the least relevant. Judge relevance by how directly and completely each
document answers or supports the query. Every identifier given in the input must
appear exactly once in the output.

## Output Format (strict JSON, no markdown fencing)
{"ranking": ["most-relevant-id", "next-id", "least-relevant-id"]}
`

// Rerank implements ingest.Reranker.
func (r *LLMReranker) Rerank(ctx context.Context, query string, results []*index.SearchResult) ([]*index.SearchResult, error) {
	if len(results) <= 1 {
		return results, nil
	}

	systemPrompt, err := prompt.Template(defaultRerankerPrompt, struct{}{})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	userPrompt, err := r.getUserPrompt(ctx, query, results)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	seed, err := text.IntHash(systemPrompt + query)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	ctx = slogx.WithAttrs(ctx, slog.Int("seed", seed))

	completion, err := r.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(
			llm.NewResponseSchema(
				"RerankedResults",
				"The document identifiers ordered from most to least relevant",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"ranking": map[string]any{
							"type":        "array",
							"description": "Document identifiers ordered from most to least relevant",
							"items": map[string]any{
								"type": "string",
							},
						},
					},
					"required":             []string{"ranking"},
					"additionalProperties": false,
				},
			),
		),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, systemPrompt),
			llm.NewMessage(llm.RoleUser, userPrompt),
		),
		llm.WithSeed(seed),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	type llmResponse struct {
		Ranking []string `json:"ranking"`
	}

	responses, err := llm.ParseJSON[llmResponse](completion.Message())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Build the section rank map from the (possibly multiple) responses,
	// keeping the first position seen for each identifier.
	rank := map[model.SectionID]int{}
	next := 0
	for _, resp := range responses {
		for _, id := range resp.Ranking {
			sectionID := model.SectionID(id)
			if _, seen := rank[sectionID]; seen {
				continue
			}
			rank[sectionID] = next
			next++
		}
	}

	slog.DebugContext(ctx, "reranker ranking", slog.Int("rankedSections", len(rank)))

	return applyRanking(results, rank), nil
}

// applyRanking reorders results and their sections according to the section
// ranks. Sections and results the LLM did not rank keep their prior relative
// order at the tail. Scores are rewritten to reflect the new ordering so that
// downstream sorting and pagination stay consistent.
func applyRanking(results []*index.SearchResult, rank map[model.SectionID]int) []*index.SearchResult {
	const unranked = 1 << 30

	sectionRank := func(id model.SectionID) int {
		if r, ok := rank[id]; ok {
			return r
		}
		return unranked
	}

	// Result rank = best (lowest) rank among its sections.
	resultRank := func(r *index.SearchResult) int {
		best := unranked
		for _, s := range r.Sections {
			if sr := sectionRank(s); sr < best {
				best = sr
			}
		}
		return best
	}

	out := make([]*index.SearchResult, len(results))
	copy(out, results)

	for _, r := range out {
		sections := make([]model.SectionID, len(r.Sections))
		copy(sections, r.Sections)
		slices.SortStableFunc(sections, func(a, b model.SectionID) int {
			return sectionRank(a) - sectionRank(b)
		})
		r.Sections = sections

		scores := make(map[model.SectionID]float64, len(sections))
		for _, s := range sections {
			// Higher score for better (lower) rank; unranked sections get ~0.
			scores[s] = 1.0 / float64(1+sectionRank(s))
		}
		r.SectionScores = scores
	}

	slices.SortStableFunc(out, func(a, b *index.SearchResult) int {
		return resultRank(a) - resultRank(b)
	})

	for _, r := range out {
		r.Score = 1.0 / float64(1+resultRank(r))
	}

	return out
}

func (r *LLMReranker) getUserPrompt(ctx context.Context, query string, results []*index.SearchResult) (string, error) {
	sectionsMap, err := r.store.GetSectionsByIDs(ctx, allSectionIDs(results))
	if err != nil {
		return "", errors.WithStack(err)
	}

	maxTotalWords := r.maxTotalWords
	if maxTotalWords <= 0 {
		maxTotalWords = 8000
	}
	totalWords := 0

	var sb strings.Builder
	sb.WriteString("## Query\n\n")
	sb.WriteString(query)
	sb.WriteString("\n\n## Documents\n\n")

	for _, res := range results {
		for _, s := range res.Sections {
			section, exists := sectionsMap[s]
			if !exists {
				continue
			}

			if totalWords >= maxTotalWords {
				break
			}

			sb.WriteString("### Document ")
			sb.WriteString(string(section.ID()))
			sb.WriteString("\n\n**Identifier:** ")
			sb.WriteString(string(section.ID()))
			sb.WriteString("\n\n")

			content, err := section.Content()
			if err != nil {
				return "", errors.WithStack(err)
			}

			text, consumed := truncateSection(string(content), r.maxSectionWords, maxTotalWords-totalWords)
			totalWords += consumed

			sb.WriteString(text)
			sb.WriteString("\n\n")
		}
	}

	return sb.String(), nil
}

// NewLLMReranker builds an LLM-backed reranker. maxTotalWords bounds the total
// prompt budget (defaults to 8000 when <= 0); each section is additionally
// capped by WithMaxSectionWords.
func NewLLMReranker(client llm.Client, store SectionStore, maxTotalWords int, funcs ...PromptOption) *LLMReranker {
	opts := newPromptOptions(funcs...)
	return &LLMReranker{
		llm:             client,
		store:           store,
		maxTotalWords:   maxTotalWords,
		maxSectionWords: opts.maxSectionWords,
	}
}
