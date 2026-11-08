package cli

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval/indecisrel"
	"github.com/bornholm/amoxtli/retrieval/relevancedata"
	"github.com/bornholm/indecis"
	"github.com/bornholm/indecis/dataset"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newRelevanceCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relevance",
		Short: "Build a local relevance model for this corpus",
		Long: "A relevance model reranks the search results on the CPU, without any chat call.\n" +
			"It is built once per corpus: \"dataset\" has the chat LLM label (query, passage)\n" +
			"pairs drawn from the indexed documents, \"train\" fine-tunes a base model on them.\n" +
			"Point retrieval.relevance.model at the result to use it.",
	}

	cmd.AddCommand(newRelevanceDatasetCommand(opts), newRelevanceTrainCommand())

	return cmd
}

func newRelevanceDatasetCommand(opts *rootOptions) *cobra.Command {
	var (
		output      string
		sections    int
		minWords    int
		candidates  int
		concurrency int
		seed        uint64
	)

	cmd := &cobra.Command{
		Use:   "dataset",
		Short: "Have the chat LLM label (query, passage) pairs drawn from the corpus",
		Long: "For each sampled section, the chat LLM writes a query the section answers; the\n" +
			"query is searched, and the LLM judges which retrieved sections are relevant.\n" +
			"Costs two chat calls per section.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				ctx := cmd.Context()
				if rt.LLM == nil {
					return errors.New("relevance dataset: llm.chat must be configured to label the pairs")
				}
				if sections < 1 || candidates < 1 || concurrency < 1 || minWords < 0 {
					return errors.New("relevance dataset: --sections, --candidates and --concurrency must be at least 1, --min-words not negative")
				}

				sample, err := relevancedata.Sample(relevancedata.WalkStore(ctx, rt.Store), sections,
					relevancedata.All(relevancedata.Leaves(), relevancedata.MinWords(minWords)), seed)
				if err != nil {
					return errors.WithStack(err)
				}
				if len(sample) == 0 {
					return errors.New("relevance dataset: no section to draw queries from")
				}

				// Candidates come from the plain hybrid search: no LLM stage,
				// and no relevance model already configured. The searches are
				// serialized: the local vector index (sqlite-vec under wasm)
				// does not survive concurrent queries. Only the LLM calls
				// overlap.
				var searchMu sync.Mutex
				search := func(ctx context.Context, query string, maxResults int, collections []model.CollectionID) ([]*index.SearchResult, error) {
					searchMu.Lock()
					defer searchMu.Unlock()
					return rt.Codex.Search(ctx, query,
						amoxtli.WithSearchMaxResults(maxResults),
						amoxtli.WithSearchProfile(amoxtli.ProfileFast))
				}

				builder := relevancedata.NewBuilder(rt.LLM, rt.Store, search, relevancedata.Options{
					Candidates:  candidates,
					Concurrency: concurrency,
					Seed:        seed,
				})
				pairs, stats, err := builder.Build(ctx, sample)
				if err != nil {
					return errors.WithStack(err)
				}

				f, err := os.Create(output)
				if err != nil {
					return errors.WithStack(err)
				}
				defer f.Close()
				if err := relevancedata.WriteJSONL(f, pairs); err != nil {
					return errors.WithStack(err)
				}

				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s, written to %s\n", stats, output)
				return nil
			})
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&output, "output", "o", "relevance-pairs.jsonl", "file the pairs are written to (JSONL)")
	flags.IntVarP(&sections, "sections", "n", 800, "sections to draw a query from")
	flags.IntVar(&minWords, "min-words", 30, "ignore the sections shorter than this")
	flags.IntVar(&candidates, "candidates", 10, "retrieved sections labeled per query")
	flags.IntVar(&concurrency, "concurrency", 4, "queries labeled in parallel")
	flags.Uint64Var(&seed, "seed", 1, "seed of the sampling and of the query styles")

	return cmd
}

func newRelevanceTrainCommand() *cobra.Command {
	var (
		base   string
		output string
		extra  string
		epochs int
		seed   int64
	)

	cmd := &cobra.Command{
		Use:   "train <pairs.jsonl>...",
		Short: "Fine-tune a relevance model on labeled pairs",
		Long: "Fine-tunes the --base model (a generic relevance model, or a bare indecis\n" +
			"backbone) on the pairs written by \"amoxtli relevance dataset\". Training runs on\n" +
			"the CPU and takes from minutes to an hour depending on the number of pairs.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			var pairs []relevancedata.Pair
			for _, path := range args {
				examples, err := dataset.ReadFile(path)
				if err != nil {
					return errors.WithStack(err)
				}
				for _, e := range examples {
					pairs = append(pairs, relevancedata.Pair{Query: e.Context, Passage: e.Text, Labels: e.Labels, Family: e.Family})
				}
			}

			opts := indecisrel.TrainOptions{
				Base:   base,
				Epochs: epochs,
				Seed:   seed,
				Progress: func(p indecis.Progress) {
					if p.Step%50 == 0 || p.Step == p.Steps {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "step %d/%d  loss %.4f  %s\n", p.Step, p.Steps, p.Loss, p.Elapsed.Round(1e9))
					}
				},
			}
			if extra != "" {
				examples, err := dataset.ReadFile(extra)
				if err != nil {
					return errors.WithStack(err)
				}
				opts.Extra = examples
			}

			if err := indecisrel.Train(ctx, pairs, output, opts); err != nil {
				return errors.WithStack(err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Relevance model written to %s\n", output)
			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&base, "base", "", "model to start from (required)")
	flags.StringVarP(&output, "output", "o", "relevance-model", "directory the model is written to")
	flags.StringVar(&extra, "extra", "", "generic examples mixed in, so the base model does not forget them (JSONL)")
	flags.IntVar(&epochs, "epochs", 1, "passes over the examples")
	flags.Int64Var(&seed, "seed", 1, "seed of the training")
	_ = cmd.MarkFlagRequired("base")

	return cmd
}
