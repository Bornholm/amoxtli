package cli

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/ignore"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

// syncResult is the per-file outcome reported by "amoxtli sync".
type syncResult struct {
	File     string  `json:"file"`
	Action   string  `json:"action"` // indexed, skipped, deleted, pending, failed (or index/delete in --dry-run)
	Error    string  `json:"error,omitempty"`
	Duration float64 `json:"duration_ms,omitempty"`
}

func newSyncCommand(opts *rootOptions) *cobra.Command {
	var (
		collection string
		filters    []string
		baseDir    string
		noWait     bool
		noIgnore   bool
		dryRun     bool
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "sync <dir>",
		Short: "Synchronize the index with a directory tree",
		Long: "Recursively indexes the files under <dir> that match --filter,\n" +
			"skipping files whose content is unchanged since the last sync. Files\n" +
			"that were indexed from this tree but have since disappeared from disk\n" +
			"are removed from the index. Files still present but excluded by the\n" +
			"filter are left untouched.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			treeAbs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			info, err := os.Stat(treeAbs)
			if err != nil {
				return err
			}
			if !info.IsDir() {
				return errors.Errorf("%q is not a directory", args[0])
			}

			// Sources are stored relative to --base-dir when set, so an indexed
			// document never carries the host's absolute paths. The synced tree
			// must sit below it, otherwise no source could be derived.
			sources, err := newSourceMapper(baseDir)
			if err != nil {
				return err
			}

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			rt, err := runtime.Open(ctx, ws, cfg, "sync")
			if err != nil {
				return err
			}
			defer rt.Close()

			collID, err := rt.ResolveCollection(ctx, collection, true)
			if err != nil {
				return err
			}

			supported := rt.Codex.Manager().SupportedExtensions()

			// .amoxtlignore rules are anchored at the workspace root and applied
			// to every walked entry unless --no-ignore is set.
			var ign *ignore.Matcher
			if !noIgnore {
				ign = ignore.New(ws.Root)
			}

			// Current index state for this subtree, keyed by source URL. Serves
			// both the ETag skip and the deletion detection below.
			prefix, err := sources.DirPrefix(treeAbs)
			if err != nil {
				return err
			}
			indexed, err := loadIndexedDigests(ctx, rt, prefix)
			if err != nil {
				return err
			}

			// Phase 1: walk the tree, deciding for each matching file whether it
			// is unchanged (skip) or new/modified (schedule). Sources seen on
			// disk are recorded so the deletion phase can spot the missing ones.
			seen := make(map[string]struct{})
			var toSchedule []string
			skipped := 0

			walkErr := filepath.WalkDir(treeAbs, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}

				if d.IsDir() {
					// Prune ignored directories (e.g. .git, node_modules) so we
					// never descend into them.
					if ign != nil {
						if ok, _, ierr := ign.Ignored(path); ierr == nil && ok {
							return filepath.SkipDir
						}
					}
					return nil
				}

				if ign != nil {
					if ok, _, ierr := ign.Ignored(path); ierr == nil && ok {
						return nil
					}
				}
				if !slices.Contains(supported, strings.ToLower(filepath.Ext(path))) {
					return nil
				}
				if !matchesFilter(filters, filepath.Base(path)) {
					return nil
				}

				fi, ierr := d.Info()
				if ierr != nil {
					return ierr
				}

				sourceURL, serr := sources.Source(path)
				if serr != nil {
					return serr
				}

				source := sourceURL.String()
				seen[source] = struct{}{}

				// mtime+size ETag, identical to "amoxtli add": an unchanged file
				// keeps the same value and is skipped.
				etag := fmt.Sprintf("%x-%x", fi.ModTime().Unix(), fi.Size())
				if prev, ok := indexed[source]; ok && prev == etag {
					skipped++
					return nil
				}

				toSchedule = append(toSchedule, path)
				return nil
			})
			if walkErr != nil {
				return errors.Wrap(walkErr, "could not walk directory")
			}

			// Deletion candidates: indexed sources under this tree that were not
			// seen on disk AND whose file no longer exists. A file still present
			// but excluded by --filter is not "seen" yet still on disk, so it is
			// deliberately kept.
			var toDelete []string
			for source := range indexed {
				if _, ok := seen[source]; ok {
					continue
				}
				u, perr := url.Parse(source)
				if perr != nil {
					continue
				}
				if _, serr := os.Stat(sources.Path(u)); os.IsNotExist(serr) {
					toDelete = append(toDelete, source)
				}
			}

			results := make([]syncResult, 0, len(toSchedule)+len(toDelete))

			if dryRun {
				for _, p := range toSchedule {
					results = append(results, syncResult{File: p, Action: "index"})
				}
				for _, s := range toDelete {
					results = append(results, syncResult{File: s, Action: "delete"})
				}

				if !opts.json {
					out := cmd.OutOrStdout()
					for _, r := range results {
						_, _ = fmt.Fprintf(out, "%s: would %s\n", r.File, r.Action)
					}
					_, _ = fmt.Fprintf(out, "\nDry run for %s: would index %d, would delete %d, %d unchanged.\n",
						treeAbs, len(toSchedule), len(toDelete), skipped)
				}

				return syncOutput(cmd, opts, treeAbs, results, len(toSchedule), skipped, len(toDelete), 0)
			}

			// Phase 2: schedule the new/modified files so the task runner indexes
			// them concurrently, then resolve the results in order.
			scheduled := make([]scheduledFile, len(toSchedule))
			for i, path := range toSchedule {
				scheduled[i] = scheduleFile(cmd, rt, collID, path, supported, sources, nil)
			}

			indexedCount := 0
			failures := 0
			for i := range scheduled {
				res := resolveScheduled(ctx, rt, &scheduled[i], !noWait, timeout)

				action := "failed"
				switch {
				case res.Status == string(task.StatusSucceeded):
					action = "indexed"
					indexedCount++
				case noWait && res.Status == string(task.StatusPending):
					action = "pending"
					indexedCount++
				default:
					failures++
				}

				results = append(results, syncResult{File: res.File, Action: action, Error: res.Error, Duration: res.Duration})

				if !opts.json {
					line := fmt.Sprintf("%s: %s", res.File, action)
					if res.Error != "" {
						line += " (" + res.Error + ")"
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
				}
			}

			// Phase 3: purge documents whose files disappeared. DeleteBySource
			// removes them from both the index and the store.
			deleted := 0
			for _, s := range toDelete {
				u, perr := url.Parse(s)
				if perr != nil {
					return errors.Wrapf(perr, "invalid source %q", s)
				}
				if err := rt.Codex.DeleteBySource(ctx, u); err != nil {
					return errors.Wrapf(err, "could not delete %q", s)
				}
				deleted++

				results = append(results, syncResult{File: s, Action: "deleted"})
				if !opts.json {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: deleted\n", s)
				}
			}

			if !opts.json {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nSynced %s: %d indexed, %d skipped, %d deleted, %d failed.\n",
					treeAbs, indexedCount, skipped, deleted, failures)
			}

			if err := syncOutput(cmd, opts, treeAbs, results, indexedCount, skipped, deleted, failures); err != nil {
				return err
			}

			if failures > 0 {
				return errors.Errorf("%d file(s) failed to index", failures)
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&collection, "collection", "c", "default", "target collection (label or ID, created if missing)")
	flags.StringArrayVar(&filters, "filter", nil, "only index files whose name matches this glob (e.g. '*.go', repeatable)")
	flags.StringVar(&baseDir, "base-dir", "", "store sources relative to this directory instead of their absolute path (must contain <dir>)")
	flags.BoolVar(&noWait, "no-wait", false, "schedule indexing without waiting for completion")
	flags.BoolVar(&noIgnore, "no-ignore", false, "index files even if they match a .amoxtlignore rule")
	flags.BoolVar(&dryRun, "dry-run", false, "report what would be indexed and deleted without changing anything")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to wait per file (0 = no timeout)")

	return cmd
}

// matchesFilter reports whether name matches any of the glob patterns. An empty
// pattern set matches everything. Patterns are matched against the file's base
// name only (e.g. "*.go"), like a shell glob.
func matchesFilter(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if ok, err := filepath.Match(p, name); err == nil && ok {
			return true
		}
	}

	return false
}

// loadIndexedDigests collects every (source -> ETag) pair for documents whose
// source starts with prefix, paginating the store's digest listing.
func loadIndexedDigests(ctx context.Context, rt *runtime.Runtime, prefix string) (map[string]string, error) {
	const pageSize = 500

	out := make(map[string]string)
	for page := 0; ; page++ {
		digests, err := rt.Store.ListDocumentDigests(ctx, prefix, page, pageSize)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		for _, d := range digests {
			out[d.Source] = d.ETag
		}
		if len(digests) < pageSize {
			return out, nil
		}
	}
}

// syncOutput emits the machine-readable JSON summary when --json is set. dir is
// the synchronized tree, reported as-is: it describes the local run, not what
// the index stores.
func syncOutput(cmd *cobra.Command, opts *rootOptions, dir string, results []syncResult, indexed, skipped, deleted, failed int) error {
	if !opts.json {
		return nil
	}

	return printJSON(cmd.OutOrStdout(), map[string]any{
		"base_dir": dir,
		"indexed":  indexed,
		"skipped":  skipped,
		"deleted":  deleted,
		"failed":   failed,
		"results":  results,
	})
}
