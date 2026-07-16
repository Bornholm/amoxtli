package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/bornholm/amoxtli/ingest"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newCollectionCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "collection",
		Aliases: []string{"collections", "coll"},
		Short:   "Manage collections",
	}

	cmd.AddCommand(
		newCollectionCreateCommand(opts),
		newCollectionListCommand(opts),
		newCollectionShowCommand(opts),
		newCollectionRenameCommand(opts),
		newCollectionDescribeCommand(opts),
		newCollectionStatsCommand(opts),
		newCollectionDeleteCommand(opts),
	)

	return cmd
}

type collectionInfo struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

func toCollectionInfo(coll model.Collection) collectionInfo {
	return collectionInfo{
		ID:          string(coll.ID()),
		Label:       coll.Label(),
		Description: coll.Description(),
	}
}

// withRuntime is a small helper wrapping the load-config / open / defer-close
// boilerplate shared by every collection subcommand.
func withRuntime(opts *rootOptions, cmd *cobra.Command, fn func(rt *runtime.Runtime) error) error {
	ws, cfg, err := opts.loadConfig()
	if err != nil {
		return err
	}

	rt, err := runtime.Open(cmd.Context(), ws, cfg, "collection")
	if err != nil {
		return err
	}
	defer rt.Close()

	return fn(rt)
}

func newCollectionCreateCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "create <label>",
		Short: "Create a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.Codex.CreateCollection(cmd.Context(), args[0])
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), collectionInfo{ID: string(id), Label: args[0]})
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Created collection %s (%s)\n", args[0], id)

				return nil
			})
		},
	}
}

func newCollectionListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List collections",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				colls, err := rt.Store.QueryCollections(cmd.Context(), ingest.QueryCollectionsOptions{})
				if err != nil {
					return errors.WithStack(err)
				}

				infos := make([]collectionInfo, 0, len(colls))
				for _, coll := range colls {
					infos = append(infos, toCollectionInfo(coll))
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), infos)
				}

				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tLABEL\tDESCRIPTION")
				for _, info := range infos {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", info.ID, info.Label, info.Description)
				}
				tw.Flush()

				return nil
			})
		},
	}
}

func newCollectionShowCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.ResolveCollection(cmd.Context(), args[0], false)
				if err != nil {
					return err
				}

				coll, err := rt.Store.GetCollectionByID(cmd.Context(), id, false)
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), toCollectionInfo(coll))
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "ID:          %s\n", coll.ID())
				fmt.Fprintf(out, "Label:       %s\n", coll.Label())
				fmt.Fprintf(out, "Description: %s\n", coll.Description())

				return nil
			})
		},
	}
}

func newCollectionRenameCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <id> <label>",
		Short: "Rename a collection",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.ResolveCollection(cmd.Context(), args[0], false)
				if err != nil {
					return err
				}

				label := args[1]
				if _, err := rt.Store.UpdateCollection(cmd.Context(), id, ingest.CollectionUpdates{Label: &label}); err != nil {
					return errors.WithStack(err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Renamed collection %s to %q\n", id, label)

				return nil
			})
		},
	}
}

func newCollectionDescribeCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "describe <id> <description>",
		Short: "Set a collection's description",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.ResolveCollection(cmd.Context(), args[0], false)
				if err != nil {
					return err
				}

				description := args[1]
				if _, err := rt.Store.UpdateCollection(cmd.Context(), id, ingest.CollectionUpdates{Description: &description}); err != nil {
					return errors.WithStack(err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Updated description of collection %s\n", id)

				return nil
			})
		},
	}
}

func newCollectionStatsCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "stats <id>",
		Short: "Show a collection's statistics",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.ResolveCollection(cmd.Context(), args[0], false)
				if err != nil {
					return err
				}

				stats, err := rt.Store.GetCollectionStats(cmd.Context(), id)
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), map[string]any{"id": id, "total_documents": stats.TotalDocuments})
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Collection %s: %d document(s)\n", id, stats.TotalDocuments)

				return nil
			})
		},
	}
}

func newCollectionDeleteCommand(opts *rootOptions) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "delete <id>",
		Short: "Delete a collection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				id, err := rt.ResolveCollection(cmd.Context(), args[0], false)
				if err != nil {
					return err
				}

				if !yes {
					confirmed, err := confirm(cmd, fmt.Sprintf("Delete collection %s? Its documents become orphaned.", id))
					if err != nil {
						return err
					}
					if !confirmed {
						fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
						return nil
					}
				}

				if err := rt.Store.DeleteCollection(cmd.Context(), id); err != nil {
					return errors.WithStack(err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Deleted collection %s. Run \"amoxtli cleanup\" to prune orphaned documents.\n", id)

				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")

	return cmd
}
