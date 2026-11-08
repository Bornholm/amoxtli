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
	"slices"

	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/amoxtli/retrieval/relevancedata"
	"github.com/bornholm/indecis"
	"github.com/pkg/errors"
)

// Question names the model must answer.
const (
	QuestionRelevant   = relevancedata.QuestionRelevant
	QuestionSufficient = relevancedata.QuestionSufficient
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
	if err := checkSchema(m.Schema()); err != nil {
		return nil, errors.Wrapf(err, "indecisrel: %s", dir)
	}
	return &Model{model: m}, nil
}

// checkSchema makes sure the model answers the questions Score reads: a
// missing one would read as zero, silently.
func checkSchema(schema indecis.Schema) error {
	var relevant, sufficient bool
	for _, q := range schema {
		switch q.Name {
		case QuestionRelevant:
			relevant = q.Kind == indecis.Noul
		case QuestionSufficient:
			sufficient = q.Kind == indecis.Score &&
				slices.Contains(q.Options, relevancedata.SufficiencyNone) &&
				slices.Contains(q.Options, relevancedata.SufficiencyPartial) &&
				slices.Contains(q.Options, relevancedata.SufficiencyComplete)
		}
	}
	if !relevant || !sufficient {
		return errors.Errorf("the model must answer %q (yes/no) and %q (%s, %s, %s)", QuestionRelevant, QuestionSufficient,
			relevancedata.SufficiencyNone, relevancedata.SufficiencyPartial, relevancedata.SufficiencyComplete)
	}
	return nil
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
			Complete: sufficiency[relevancedata.SufficiencyComplete],
			Partial:  sufficiency[relevancedata.SufficiencyPartial],
		}
	}

	return scores, nil
}

var _ retrieval.RelevanceModel = &Model{}
