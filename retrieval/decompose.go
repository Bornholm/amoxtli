package retrieval

import (
	"context"
	"log/slog"
	"strings"

	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/genai/llm"
	"github.com/bornholm/genai/llm/prompt"
	"github.com/bornholm/go-x/slogx"
	"github.com/pkg/errors"
)

// QueryDecomposer splits a complex, multi-hop question into a small set of
// standalone sub-questions whose combined evidence answers the original. It
// powers query decomposition (ported from MothRAG).
type QueryDecomposer interface {
	Decompose(ctx context.Context, query string) ([]string, error)
}

const defaultDecomposePrompt = `
You are a question decomposition assistant for a retrieval-augmented answering
system.

Break the user's question into at most {{ .MaxSubQueries }} standalone
sub-questions, each of which can be searched independently and whose combined
answers are enough to answer the original question. Each sub-question must be
self-contained (no pronouns referring to the others). If the question is already
atomic, return it unchanged as a single sub-question.

## Output Format (strict JSON, no markdown fencing)
{"subqueries": ["sub-question 1", "sub-question 2"]}
`

// LLMQueryDecomposer implements QueryDecomposer with a single structured-JSON
// LLM call.
type LLMQueryDecomposer struct {
	llm           llm.Client
	maxSubQueries int
}

// NewLLMQueryDecomposer builds a decomposer. maxSubQueries bounds the number of
// sub-questions returned (defaults to 3 when <= 0).
func NewLLMQueryDecomposer(client llm.Client, maxSubQueries int) *LLMQueryDecomposer {
	if maxSubQueries <= 0 {
		maxSubQueries = 3
	}
	return &LLMQueryDecomposer{
		llm:           client,
		maxSubQueries: maxSubQueries,
	}
}

// Decompose implements QueryDecomposer. It never returns an empty slice on
// success: a query that cannot be decomposed yields the original query.
func (d *LLMQueryDecomposer) Decompose(ctx context.Context, query string) ([]string, error) {
	systemPrompt, err := prompt.Template(defaultDecomposePrompt, struct {
		MaxSubQueries int
	}{
		MaxSubQueries: d.maxSubQueries,
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	seed, err := text.IntHash(systemPrompt + query)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	ctx = slogx.WithAttrs(ctx, slog.Int("seed", seed))

	completion, err := d.llm.ChatCompletion(ctx,
		llm.WithJSONResponse(
			llm.NewResponseSchema(
				"Decomposition",
				"The standalone sub-questions the original question decomposes into",
				map[string]any{
					"type": "object",
					"properties": map[string]any{
						"subqueries": map[string]any{
							"type":        "array",
							"description": "Standalone sub-questions",
							"items":       map[string]any{"type": "string"},
						},
					},
					"required":             []string{"subqueries"},
					"additionalProperties": false,
				},
			),
		),
		llm.WithMessages(
			llm.NewMessage(llm.RoleSystem, systemPrompt),
			llm.NewMessage(llm.RoleUser, query),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	type llmResponse struct {
		SubQueries []string `json:"subqueries"`
	}

	responses, err := llm.ParseJSON[llmResponse](completion.Message())
	if err != nil {
		return nil, errors.WithStack(err)
	}

	subQueries := make([]string, 0, d.maxSubQueries)
	seen := map[string]struct{}{}
	for _, r := range responses {
		for _, sq := range r.SubQueries {
			sq = strings.TrimSpace(sq)
			if sq == "" {
				continue
			}
			if _, exists := seen[sq]; exists {
				continue
			}
			seen[sq] = struct{}{}
			subQueries = append(subQueries, sq)
			if len(subQueries) >= d.maxSubQueries {
				break
			}
		}
		if len(subQueries) >= d.maxSubQueries {
			break
		}
	}

	// Fail-safe: never return an empty decomposition.
	if len(subQueries) == 0 {
		subQueries = []string{query}
	}

	slog.DebugContext(ctx, "query decomposition", slog.Any("subqueries", subQueries))

	return subQueries, nil
}

var _ QueryDecomposer = &LLMQueryDecomposer{}
