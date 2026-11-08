package indecisrel

import (
	"context"
	"log/slog"
	"time"

	"github.com/bornholm/amoxtli/retrieval/relevancedata"
	"github.com/bornholm/indecis"
	"github.com/bornholm/indecis/dataset"
	"github.com/pkg/errors"
)

// Schema returns the questions of a relevance model.
func Schema() indecis.Schema {
	return indecis.Schema{
		indecis.NewNoul(QuestionRelevant, "The passage answers the query or brings context needed to answer it."),
		indecis.NewScore(QuestionSufficient, "The passages are enough to answer the query without outside knowledge.",
			relevancedata.SufficiencyNone, relevancedata.SufficiencyPartial, relevancedata.SufficiencyComplete),
	}
}

// TrainOptions configures Train.
type TrainOptions struct {
	// Base is the directory of the model to start from: a relevance model
	// trained on generic data, or a bare backbone.
	Base string
	// Extra examples mixed with the pairs, typically a sample of the generic
	// data the base model was trained on, so that it does not forget it.
	Extra []dataset.Example
	// Epochs over the examples (default 1).
	Epochs int
	// MaxLen is the maximum number of tokens read per pair (default 384).
	MaxLen int
	// Seed of the training.
	Seed int64
	// Progress, if set, receives the training progress.
	Progress func(indecis.Progress)
}

// Train fine-tunes the base model on the pairs and the extra examples, sets
// its temperatures on 5% of them held out by query, and writes it to out.
func Train(ctx context.Context, pairs []relevancedata.Pair, out string, opts TrainOptions) error {
	if opts.Epochs <= 0 {
		opts.Epochs = 1
	}
	if opts.MaxLen <= 0 {
		opts.MaxLen = 384
	}

	examples := make([]dataset.Example, 0, len(pairs)+len(opts.Extra))
	for _, p := range pairs {
		examples = append(examples, dataset.Example{
			Context: p.Query,
			Text:    p.Passage,
			Labels:  p.Labels,
			Family:  p.Family,
		})
	}
	examples = append(examples, opts.Extra...)
	if len(examples) == 0 {
		return errors.New("indecisrel: no examples to train on")
	}

	train, calib := dataset.HoldOut(examples, 0.05, uint64(opts.Seed)+1)

	m, err := indecis.New(opts.Base, Schema(), opts.Seed, indecis.WithMaxLen(opts.MaxLen), indecis.WithPairs())
	if err != nil {
		return errors.WithStack(err)
	}

	to := indecis.DefaultTrainOptions()
	to.Epochs, to.Seed, to.Progress = opts.Epochs, opts.Seed, opts.Progress

	start := time.Now()
	if err := m.Fit(ctx, dataset.Shuffle(train, opts.Seed), to); err != nil {
		return errors.WithStack(err)
	}
	if len(calib) > 0 {
		if _, err := m.Calibrate(ctx, calib); err != nil {
			return errors.WithStack(err)
		}
	}
	slog.InfoContext(ctx, "relevance model trained",
		slog.Int("examples", len(train)), slog.Duration("elapsed", time.Since(start).Round(time.Second)))

	return errors.WithStack(m.Save(out))
}
