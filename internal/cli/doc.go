package cli

import (
	"context"
	"fmt"
	"net/url"
	"text/tabwriter"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newDocCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "doc",
		Aliases: []string{"docs", "document"},
		Short:   "Inspect and manage indexed documents",
	}

	cmd.AddCommand(
		newDocListCommand(opts),
		newDocShowCommand(opts),
		newDocDeleteCommand(opts),
	)

	return cmd
}

type docInfo struct {
	ID          string         `json:"id"`
	Source      string         `json:"source"`
	ETag        string         `json:"etag,omitempty"`
	Collections []string       `json:"collections,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	CreatedAt   string         `json:"created_at,omitempty"`
}

func toDocInfo(doc model.PersistedDocument) docInfo {
	info := docInfo{
		ID:        string(doc.ID()),
		ETag:      doc.ETag(),
		CreatedAt: doc.CreatedAt().Format("2006-01-02 15:04:05"),
		Metadata:  model.Metadata(doc),
	}
	if doc.Source() != nil {
		info.Source = doc.Source().String()
	}
	for _, coll := range doc.Collections() {
		info.Collections = append(info.Collections, coll.Label())
	}

	return info
}

// queryOptions builds the store query from the shared listing flags.
func queryOptions(page, limit int, orphaned bool, sourceLike, sortBy, order string, headerOnly bool) ingest.QueryDocumentsOptions {
	opts := ingest.QueryDocumentsOptions{HeaderOnly: headerOnly}

	if page > 0 {
		opts.Page = &page
	}
	if limit > 0 {
		opts.Limit = &limit
	}
	if orphaned {
		v := true
		opts.Orphaned = &v
	}
	if sourceLike != "" {
		opts.SourcePattern = &sourceLike
	}
	if sortBy != "" {
		opts.SortBy = &sortBy
	}
	if order != "" {
		opts.SortOrder = &order
	}

	return opts
}

func newDocListCommand(opts *rootOptions) *cobra.Command {
	var (
		collection string
		orphaned   bool
		sourceLike string
		page       int
		limit      int
		sortBy     string
		order      string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List indexed documents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			rt, err := runtime.Open(ctx, ws, cfg, "doc")
			if err != nil {
				return err
			}
			defer rt.Close()

			query := queryOptions(page, limit, orphaned, sourceLike, sortBy, order, true)

			var (
				docs  []model.PersistedDocument
				total int64
			)

			if collection != "" {
				collID, err := rt.ResolveCollection(ctx, collection, false)
				if err != nil {
					return err
				}
				docs, total, err = rt.Store.QueryDocumentsByCollectionID(ctx, collID, query)
				if err != nil {
					return errors.WithStack(err)
				}
			} else {
				docs, total, err = rt.Store.QueryDocuments(ctx, query)
				if err != nil {
					return errors.WithStack(err)
				}
			}

			infos := make([]docInfo, 0, len(docs))
			for _, doc := range docs {
				infos = append(infos, toDocInfo(doc))
			}

			if opts.json {
				return printJSON(cmd.OutOrStdout(), map[string]any{"total": total, "documents": infos})
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tSOURCE\tCREATED")
			for _, info := range infos {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", info.ID, info.Source, info.CreatedAt)
			}
			_ = tw.Flush()
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\n%d of %d document(s).\n", len(infos), total)

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&collection, "collection", "c", "", "restrict to a collection (label or ID)")
	flags.BoolVar(&orphaned, "orphaned", false, "only documents without a collection")
	flags.StringVar(&sourceLike, "source-like", "", "only documents whose source matches this pattern")
	flags.IntVar(&page, "page", 0, "page number (1-based)")
	flags.IntVar(&limit, "limit", 50, "page size")
	flags.StringVar(&sortBy, "sort", "", "sort column: source or created_at")
	flags.StringVar(&order, "order", "", "sort order: asc or desc")

	return cmd
}

func newDocShowCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a document's details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			rt, err := runtime.Open(ctx, ws, cfg, "doc")
			if err != nil {
				return err
			}
			defer rt.Close()

			doc, err := rt.Store.GetDocumentByID(ctx, model.DocumentID(args[0]))
			if err != nil {
				return errors.WithStack(err)
			}

			info := toDocInfo(doc)

			if opts.json {
				return printJSON(cmd.OutOrStdout(), info)
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "ID:          %s\n", info.ID)
			_, _ = fmt.Fprintf(out, "Source:      %s\n", info.Source)
			_, _ = fmt.Fprintf(out, "ETag:        %s\n", info.ETag)
			_, _ = fmt.Fprintf(out, "Created:     %s\n", info.CreatedAt)
			if len(info.Collections) > 0 {
				_, _ = fmt.Fprintf(out, "Collections: %v\n", info.Collections)
			}
			if len(info.Metadata) > 0 {
				_, _ = fmt.Fprintf(out, "Metadata:    %v\n", info.Metadata)
			}
			_, _ = fmt.Fprintf(out, "Sections:    %d\n", model.CountSections(doc))

			return nil
		},
	}

	return cmd
}

func newDocDeleteCommand(opts *rootOptions) *cobra.Command {
	var (
		collection string
		orphaned   bool
		source     string
		sourceLike string
		dryRun     bool
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "delete [<id>...]",
		Short: "Delete documents by ID or by filter (batch)",
		Long:  "Deletes documents from both the store and the index. Provide explicit\nIDs, or select a batch with --source, --source-like, --orphaned or\n--collection.",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			hasFilter := collection != "" || orphaned || source != "" || sourceLike != ""
			if len(args) == 0 && !hasFilter {
				return errors.New("provide document IDs or a selection filter (--source, --source-like, --orphaned, --collection)")
			}

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			rt, err := runtime.Open(ctx, ws, cfg, "doc")
			if err != nil {
				return err
			}
			defer rt.Close()

			sources, err := collectDeletionSources(ctx, rt, args, collection, orphaned, source, sourceLike)
			if err != nil {
				return err
			}

			if len(sources) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No matching document.")
				return nil
			}

			if dryRun {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Would delete %d document(s):\n", len(sources))
				for _, s := range sources {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", s)
				}
				return nil
			}

			if !yes {
				confirmed, err := confirm(cmd, fmt.Sprintf("Delete %d document(s)?", len(sources)))
				if err != nil {
					return err
				}
				if !confirmed {
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
					return nil
				}
			}

			deleted := 0
			for _, s := range sources {
				u, err := url.Parse(s)
				if err != nil {
					return errors.Wrapf(err, "invalid source %q", s)
				}
				// DeleteBySource purges both the index and the store, unlike a
				// store-only delete which would leave stale index entries.
				if err := rt.Codex.DeleteBySource(ctx, u); err != nil {
					return errors.Wrapf(err, "could not delete %q", s)
				}
				deleted++
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted %d document(s).\n", deleted)

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&collection, "collection", "c", "", "delete documents in a collection (label or ID)")
	flags.BoolVar(&orphaned, "orphaned", false, "delete documents without a collection")
	flags.StringVar(&source, "source", "", "delete the document with this exact source URL")
	flags.StringVar(&sourceLike, "source-like", "", "delete documents whose source matches this pattern")
	flags.BoolVar(&dryRun, "dry-run", false, "list matching documents without deleting")
	flags.BoolVar(&yes, "yes", false, "do not ask for confirmation")

	return cmd
}

// collectDeletionSources resolves the set of source URLs to delete, from
// explicit IDs and/or a selection filter, paginating the query.
func collectDeletionSources(ctx context.Context, rt *runtime.Runtime, ids []string, collection string, orphaned bool, source, sourceLike string) ([]string, error) {
	seen := map[string]struct{}{}
	var sources []string

	add := func(doc model.PersistedDocument) {
		if doc.Source() == nil {
			return
		}
		s := doc.Source().String()
		if _, ok := seen[s]; ok {
			return
		}
		seen[s] = struct{}{}
		sources = append(sources, s)
	}

	for _, id := range ids {
		doc, err := rt.Store.GetDocumentByID(ctx, model.DocumentID(id))
		if err != nil {
			return nil, errors.Wrapf(err, "could not resolve document %q", id)
		}
		add(doc)
	}

	if source != "" {
		u, err := url.Parse(source)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid --source %q", source)
		}
		sources = appendUnique(seen, sources, u.String())
	}

	if collection != "" || orphaned || sourceLike != "" {
		if err := eachDocument(ctx, rt, collection, orphaned, sourceLike, add); err != nil {
			return nil, err
		}
	}

	return sources, nil
}

func appendUnique(seen map[string]struct{}, sources []string, s string) []string {
	if _, ok := seen[s]; ok {
		return sources
	}
	seen[s] = struct{}{}

	return append(sources, s)
}

// eachDocument iterates every document matching the filter, page by page.
func eachDocument(ctx context.Context, rt *runtime.Runtime, collection string, orphaned bool, sourceLike string, fn func(model.PersistedDocument)) error {
	const pageSize = 200

	var collID model.CollectionID
	if collection != "" {
		id, err := rt.ResolveCollection(ctx, collection, false)
		if err != nil {
			return err
		}
		collID = id
	}

	for page := 1; ; page++ {
		query := queryOptions(page, pageSize, orphaned, sourceLike, "", "", true)

		var (
			docs []model.PersistedDocument
			err  error
		)
		if collection != "" {
			docs, _, err = rt.Store.QueryDocumentsByCollectionID(ctx, collID, query)
		} else {
			docs, _, err = rt.Store.QueryDocuments(ctx, query)
		}
		if err != nil {
			return errors.WithStack(err)
		}

		for _, doc := range docs {
			fn(doc)
		}

		if len(docs) < pageSize {
			return nil
		}
	}
}
