// Package indecisrel implements retrieval.RelevanceModel with an indecis
// model: a small encoder fine-tuned on (query, passage) pairs that runs on the
// CPU, in process, without an LLM.
//
// The model must be trained with pairs (the query as context, the passage as
// text) and answer two questions: "pertinent" (noul) and "suffisance" (score
// over aucune, partielle, complete). See scripts/relevance_data.py.
package indecisrel

import (
	"context"

	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/indecis"
	"github.com/pkg/errors"
)

// Question names the model must answer.
const (
	QuestionRelevant   = "pertinent"
	QuestionSufficient = "suffisance"
)

// Model adapts an indecis model to retrieval.RelevanceModel.
type Model struct {
	model *indecis.Model
}

// Load loads the indecis model stored in dir. Options are passed to
// indecis.Load (e.g. indecis.WithInt8()).
func Load(dir string, opts ...indecis.Option) (*Model, error) {
	m, err := indecis.Load(dir, opts...)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if !m.Paired() {
		return nil, errors.Errorf("indecisrel: %s: the model does not read (query, passage) pairs", dir)
	}
	return &Model{model: m}, nil
}

// Score implements retrieval.RelevanceModel.
func (m *Model) Score(ctx context.Context, query string, passages []string) ([]retrieval.PassageScore, error) {
	inputs := make([]indecis.Input, len(passages))
	for i, p := range passages {
		inputs[i] = indecis.Input{Context: query, Text: p}
	}

	decisions, err := m.model.DecideInputs(ctx, inputs...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	scores := make([]retrieval.PassageScore, len(decisions))
	for i, d := range decisions {
		sufficiency := d[QuestionSufficient].Probs
		scores[i] = retrieval.PassageScore{
			Relevant: d[QuestionRelevant].P,
			Complete: sufficiency["complete"],
			Partial:  sufficiency["partielle"],
		}
	}

	return scores, nil
}

var _ retrieval.RelevanceModel = &Model{}
