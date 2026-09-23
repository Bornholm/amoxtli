package amoxtli

import (
	"context"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/pkg/errors"
)

// Answer streams an answer to the query written from the sections of the
// results only, best first. The model cites them as [n], n being the Number
// of an AnswerSource; retrieval.Citations extracts the valid citations of the
// complete answer. It requires a client for StageAnswer (WithStageLLMClient)
// or a default one (WithLLMClient), and returns ErrUnsupported otherwise.
//
// The channel is closed once the answer is complete; a failure of the model
// arrives as a delta carrying an error.
func (c *Codex) Answer(ctx context.Context, query string, results []*index.SearchResult, funcs ...retrieval.AnswerOption) ([]retrieval.AnswerSource, <-chan retrieval.AnswerDelta, error) {
	if c.answerer == nil {
		return nil, nil, errors.Wrap(ErrUnsupported, "no llm client for the answer stage")
	}

	sources, deltas, err := c.answerer.Answer(ctx, query, results, funcs...)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	return sources, deltas, nil
}

// CanAnswer reports whether the Codex has a model for Answer.
func (c *Codex) CanAnswer() bool {
	return c.answerer != nil
}
