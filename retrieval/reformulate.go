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

// QueryReformulator rewrites a query that failed to retrieve sufficient evidence
// into a new query aimed at surfacing the missing information. It powers the
// iterative re-retrieval loop (ported from MothRAG's γ-driven re-retrieval).
type QueryReformulator interface {
	Reformulate(ctx context.Context, query string, hint string) (string, error)
}

const defaultReformulatePrompt = `
You are a search query optimizer for a retrieval-augmented answering system.

The following query did not retrieve enough information to answer it reliably.
Rewrite it into a single, self-contained search query that is more likely to
surface the missing evidence. Prefer explicit entity names and the specific
facts that seem to be missing. Output only the rewritten query, nothing else.

## Original query

{{ .Query }}

{{ if .Hint }}## What was missing

{{ .Hint }}
{{ end }}`

// LLMQueryReformulator implements QueryReformulator with a single LLM call.
type LLMQueryReformulator struct {
	llm llm.Client
}

func NewLLMQueryReformulator(client llm.Client) *LLMQueryReformulator {
	return &LLMQueryReformulator{llm: client}
}

// Reformulate implements QueryReformulator. hint is an optional description of
// what evidence was missing (typically the grounding verdict explanation).
func (r *LLMQueryReformulator) Reformulate(ctx context.Context, query string, hint string) (string, error) {
	systemPrompt, err := prompt.Template(defaultReformulatePrompt, struct {
		Query string
		Hint  string
	}{
		Query: query,
		Hint:  hint,
	})
	if err != nil {
		return "", errors.WithStack(err)
	}

	seed, err := text.IntHash(systemPrompt)
	if err != nil {
		return "", errors.WithStack(err)
	}

	ctx = slogx.WithAttrs(ctx, slog.Int("seed", seed))

	completion, err := r.llm.ChatCompletion(ctx,
		llm.WithMessages(
			llm.NewMessage(llm.RoleUser, systemPrompt),
		),
		llm.WithTemperature(0),
		llm.WithSeed(seed),
	)
	if err != nil {
		return "", errors.WithStack(err)
	}

	reformulated := strings.TrimSpace(completion.Message().Content())

	return reformulated, nil
}

var _ QueryReformulator = &LLMQueryReformulator{}
