package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newReindexCommand(opts *rootOptions) *cobra.Command {
	var (
		collection string
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the index from the stored documents",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				ctx := cmd.Context()

				var (
					taskID task.ID
					err    error
				)

				if collection != "" {
					collID, resolveErr := rt.ResolveCollection(ctx, collection, false)
					if resolveErr != nil {
						return resolveErr
					}
					taskID, err = rt.Codex.ReindexCollection(ctx, collID)
				} else {
					taskID, err = rt.Codex.Reindex(ctx)
				}
				if err != nil {
					return errors.WithStack(err)
				}

				return awaitMaintenanceTask(cmd, rt, taskID, timeout, "Reindex")
			})
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&collection, "collection", "c", "", "reindex a single collection (label or ID)")
	flags.DurationVar(&timeout, "timeout", 30*time.Minute, "maximum time to wait (0 = no timeout)")

	return cmd
}

func newCleanupCommand(opts *rootOptions) *cobra.Command {
	var (
		collections []string
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "cleanup",
		Short: "Prune orphaned index entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				ctx := cmd.Context()

				var collIDs []model.CollectionID
				if len(collections) > 0 {
					ids, err := rt.ResolveCollections(ctx, collections, false)
					if err != nil {
						return err
					}
					collIDs = ids
				}

				taskID, err := rt.Codex.CleanupIndex(ctx, collIDs...)
				if err != nil {
					return errors.WithStack(err)
				}

				return awaitMaintenanceTask(cmd, rt, taskID, timeout, "Cleanup")
			})
		},
	}

	flags := cmd.Flags()
	flags.StringArrayVarP(&collections, "collection", "c", nil, "restrict cleanup to collections (repeatable)")
	flags.DurationVar(&timeout, "timeout", 30*time.Minute, "maximum time to wait (0 = no timeout)")

	return cmd
}

func newBackupCommand(opts *rootOptions) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Write a snapshot of the workspace to a file or stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				reader, err := rt.Codex.Backup(cmd.Context())
				if err != nil {
					return errors.WithStack(err)
				}
				defer reader.Close()

				dst := cmd.OutOrStdout()
				if output != "" && output != "-" {
					file, err := os.Create(output)
					if err != nil {
						return errors.WithStack(err)
					}
					defer file.Close()
					dst = file
				}

				if _, err := io.Copy(dst, reader); err != nil {
					return errors.WithStack(err)
				}

				if output != "" && output != "-" {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Wrote backup to %s\n", output)
				}

				return nil
			})
		},
	}

	cmd.Flags().StringVarP(&output, "output", "o", "-", "destination file (- for stdout)")

	return cmd
}

func newRestoreCommand(opts *rootOptions) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "restore <file>",
		Short: "Restore a workspace snapshot (- for stdin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				if !yes {
					confirmed, err := confirm(cmd, "Restore will overwrite the current index. Continue?")
					if err != nil {
						return err
					}
					if !confirmed {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
						return nil
					}
				}

				src := cmd.InOrStdin()
				if args[0] != "-" {
					file, err := os.Open(args[0])
					if err != nil {
						return errors.WithStack(err)
					}
					defer file.Close()
					src = file
				}

				if err := rt.Codex.Restore(cmd.Context(), src); err != nil {
					return errors.WithStack(err)
				}

				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Restore complete.")

				return nil
			})
		},
	}

	cmd.Flags().BoolVar(&yes, "yes", false, "do not ask for confirmation")

	return cmd
}

func newCacheCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage the LLM cache (embeddings and chat completions)",
	}

	cmd.AddCommand(newCachePurgeCommand(opts), newCachePruneCommand(opts))

	return cmd
}

func newCachePurgeCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "purge",
		Short: "Delete the on-disk LLM cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			dir := ws.Resolve(cfg.LLMCachePath())
			if err := os.RemoveAll(dir); err != nil {
				return errors.WithStack(err)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Purged LLM cache at %s\n", dir)

			return nil
		},
	}
}

func newCachePruneCommand(opts *rootOptions) *cobra.Command {
	var (
		maxAge  time.Duration
		maxSize int64
	)

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove the least recently used entries of the on-disk LLM cache",
		Long: "Remove the LLM cache entries unused for longer than llm.cache.max_age, then\n" +
			"the least recently used ones until the cache fits in llm.cache.max_size.\n" +
			"Commands already do it at most once a day; this runs it now.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			limits := llmx.PruneOptions{MaxAge: cfg.LLMCacheMaxAge(), MaxSize: cfg.LLM.Cache.MaxSize}
			if cmd.Flags().Changed("max-age") {
				limits.MaxAge = maxAge
			}
			if cmd.Flags().Changed("max-size") {
				limits.MaxSize = maxSize
			}
			if limits.MaxAge < 0 || limits.MaxSize < 0 {
				return errors.New("--max-age and --max-size must not be negative")
			}

			dir := ws.Resolve(cfg.LLMCachePath())

			stats, err := llmx.PruneCache(dir, limits)
			if err != nil {
				return errors.WithStack(err)
			}

			if opts.json {
				return printJSON(cmd.OutOrStdout(), stats)
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Pruned LLM cache at %s: removed %d entries (%s), kept %d (%s)\n",
				dir, stats.Removed, humanize.IBytes(uint64(stats.Freed)), stats.Kept, humanize.IBytes(uint64(stats.Size)))

			return nil
		},
	}

	flags := cmd.Flags()
	flags.DurationVar(&maxAge, "max-age", 0, "remove the entries unused for longer than this (overrides llm.cache.max_age; 0: no age limit)")
	flags.Int64Var(&maxSize, "max-size", 0, "bound the cache size in bytes (overrides llm.cache.max_size; 0: no size limit)")

	return cmd
}

// awaitMaintenanceTask waits for a maintenance task and reports its outcome.
func awaitMaintenanceTask(cmd *cobra.Command, rt *runtime.Runtime, taskID task.ID, timeout time.Duration, label string) error {
	state, err := waitTask(cmd.Context(), rt.Codex, taskID, timeout, nil)
	if err != nil {
		return err
	}

	if state.Status == task.StatusFailed {
		if state.Error != nil {
			return errors.Errorf("%s failed: %s", label, state.Error)
		}
		return errors.Errorf("%s failed", label)
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s complete.\n", label)

	return nil
}
