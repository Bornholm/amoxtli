package cli

import (
	"context"
	"fmt"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/filterexpr"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type searchSection struct {
	ID      string `json:"id"`
	Content string `json:"content,omitempty"`
}

type searchResult struct {
	Source   string          `json:"source"`
	Score    float64         `json:"score"`
	Sections []searchSection `json:"sections"`
}

type searchOutput struct {
	Results    []searchResult             `json:"results"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	Grounding  *retrieval.GroundingResult `json:"grounding,omitempty"`
	Rounds     int                        `json:"rounds,omitempty"`
}

func newSearchCommand(opts *rootOptions) *cobra.Command {
	var (
		maxResults  int
		collections []string
		filters     []string
		cursor      string
		deep        bool
		noContent   bool
	)

	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search the indexed documents",
		Long:  "Searches the workspace index. With --deep (requires llm.chat in the\nconfiguration), runs iterative retrieval: query decomposition, grounding\ncheck and reformulation rounds.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := args[0]

			conditions, err := filterexpr.ParseFilters(filters)
			if err != nil {
				return err
			}

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			if deep && !cfg.HasChat() {
				return errors.New("--deep requires llm.chat to be configured in config.yaml")
			}
			if deep && cursor != "" {
				return errors.New("--deep does not support pagination (--cursor)")
			}

			rt, err := runtime.Open(ctx, ws, cfg, "search")
			if err != nil {
				return err
			}
			defer rt.Close()

			collIDs, err := rt.ResolveCollections(ctx, collections, false)
			if err != nil {
				return err
			}

			searchOpts := []amoxtli.SearchOption{
				amoxtli.WithSearchMaxResults(maxResults),
			}
			if len(collIDs) > 0 {
				searchOpts = append(searchOpts, amoxtli.WithSearchCollections(collIDs...))
			}
			if len(conditions) > 0 {
				searchOpts = append(searchOpts, amoxtli.WithSearchFilter(conditions...))
			}
			if cursor != "" {
				searchOpts = append(searchOpts, amoxtli.WithSearchCursor(cursor))
			}

			output := &searchOutput{}

			if deep {
				result, err := rt.Codex.SearchIterative(ctx, query, searchOpts...)
				if err != nil {
					return errors.WithStack(err)
				}

				output.Results, err = renderResults(ctx, rt, result.Results, !noContent)
				if err != nil {
					return err
				}
				output.Grounding = result.Grounding
				output.Rounds = result.Rounds
			} else {
				page, err := rt.Codex.SearchPage(ctx, query, searchOpts...)
				if errors.Is(err, amoxtli.ErrCursorFilterMismatch) {
					return errors.Errorf("--cursor was issued for a different --filter; drop --cursor to restart from the first page")
				}
				if err != nil {
					return errors.WithStack(err)
				}

				output.Results, err = renderResults(ctx, rt, page.Results, !noContent)
				if err != nil {
					return err
				}
				output.NextCursor = page.NextCursor

				// When grounding is enabled, surface the confidence verdict on a
				// plain search too. It was already computed during SearchPage, so
				// reuse it rather than paying for a second LLM evaluation.
				if cfg.Retrieval.GroundingCheck {
					output.Grounding = page.Grounding
				}
			}

			if opts.json {
				return printJSON(cmd.OutOrStdout(), output)
			}

			printSearchOutput(cmd, output)

			return nil
		},
	}

	flags := cmd.Flags()
	flags.IntVarP(&maxResults, "max-results", "n", 5, "maximum number of results")
	flags.StringArrayVarP(&collections, "collection", "c", nil, "restrict to collections (label or ID, repeatable)")
	flags.StringArrayVar(&filters, "filter", nil, "metadata filter (key=value, key!=value, key>value..., key? if set, !key if unset, repeatable)")
	flags.StringVar(&cursor, "cursor", "", "resume pagination from a previous next_cursor")
	flags.BoolVar(&deep, "deep", false, "iterative retrieval driven by an LLM (requires llm.chat)")
	flags.BoolVar(&noContent, "no-content", false, "do not fetch and display section contents")

	return cmd
}

// renderResults projects index search results into the CLI output shape,
// optionally fetching the section contents.
func renderResults(ctx context.Context, rt *runtime.Runtime, results []*index.SearchResult, withContent bool) ([]searchResult, error) {
	rendered := make([]searchResult, 0, len(results))

	var sections map[model.SectionID]model.Section
	if withContent {
		ids := []model.SectionID{}
		for _, r := range results {
			ids = append(ids, r.Sections...)
		}

		var err error
		sections, err = rt.Codex.GetSectionsByIDs(ctx, ids)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	for _, r := range results {
		result := searchResult{
			Score:    r.Score,
			Sections: make([]searchSection, 0, len(r.Sections)),
		}
		if r.Source != nil {
			result.Source = r.Source.String()
		}

		for _, id := range r.Sections {
			section := searchSection{ID: string(id)}

			if s, ok := sections[id]; ok {
				content, err := s.Content()
				if err != nil {
					return nil, errors.WithStack(err)
				}

				section.Content = string(content)
			}

			result.Sections = append(result.Sections, section)
		}

		rendered = append(rendered, result)
	}

	return rendered, nil
}

func printSearchOutput(cmd *cobra.Command, output *searchOutput) {
	out := cmd.OutOrStdout()
	p := newPalette(out)

	if len(output.Results) == 0 {
		_, _ = fmt.Fprintln(out, "No results.")
		return
	}

	for i, r := range output.Results {
		if i > 0 {
			_, _ = fmt.Fprintln(out)
		}

		title, path := formatSource(r.Source)

		_, _ = fmt.Fprintf(out, "%s%d.%s %s%s%s  %s%.3f%s\n",
			p.dim, i+1, p.reset,
			p.bold, title, p.reset,
			p.green, r.Score, p.reset,
		)

		if path != "" {
			_, _ = fmt.Fprintf(out, "   %s%s%s\n", p.dim, path, p.reset)
		}

		for _, s := range r.Sections {
			if s.Content != "" {
				_, _ = fmt.Fprintf(out, "   %s•%s %s\n", p.cyan, p.reset, excerpt(s.Content, 200))
			}
		}
	}

	if output.Grounding != nil {
		rounds := ""
		if output.Rounds > 0 {
			rounds = fmt.Sprintf(", %d extra round(s)", output.Rounds)
		}
		_, _ = fmt.Fprintf(out, "\n%sGrounding:%s %s (confidence %.2f%s)\n",
			p.bold, p.reset, output.Grounding.Status, output.Grounding.Score, rounds)
		if output.Grounding.Explanation != "" {
			_, _ = fmt.Fprintf(out, "  %s%s%s\n", p.dim, output.Grounding.Explanation, p.reset)
		}
	}

	if output.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "\n%sMore results:%s --cursor %q\n", p.dim, p.reset, output.NextCursor)
	}
}
