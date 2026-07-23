package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/filterexpr"
	"github.com/bornholm/amoxtli/internal/ignore"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

type addResult struct {
	File     string  `json:"file"`
	TaskID   string  `json:"task_id,omitempty"`
	Status   string  `json:"status"`
	Error    string  `json:"error,omitempty"`
	Duration float64 `json:"duration_ms"`
}

func newAddCommand(opts *rootOptions) *cobra.Command {
	var (
		collection string
		metaPairs  []string
		baseDir    string
		noWait     bool
		noIgnore   bool
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add <file>...",
		Short: "Index local files into the workspace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			metadata, err := filterexpr.ParseMetadata(metaPairs)
			if err != nil {
				return err
			}

			// Sources are stored relative to --base-dir when set, so an indexed
			// document never carries the host's absolute paths.
			sources, err := newSourceMapper(baseDir)
			if err != nil {
				return err
			}

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			rt, err := runtime.Open(ctx, ws, cfg, "add")
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
			// to every explicit file argument unless --no-ignore is set.
			var ign *ignore.Matcher
			if !noIgnore {
				ign = ignore.New(ws.Root)
			}

			// Phase 1: schedule every file up front so the task runner can index
			// them concurrently (up to indexing.task_parallelism) instead of one
			// at a time. Scheduling is non-blocking; the waiting happens below.
			scheduled := make([]scheduledFile, len(args))
			for i, path := range args {
				if ign != nil {
					if ok, src, err := ign.Ignored(path); err == nil && ok {
						scheduled[i] = ignoredFile(path, src)
						continue
					}
				}
				scheduled[i] = scheduleFile(cmd, rt, collID, path, supported, sources, metadata)
			}

			// Phase 2: resolve results in input order. Because the tasks are
			// already running in parallel, waiting sequentially still streams the
			// output in order while overall wall time tracks the slowest wave.
			results := make([]addResult, 0, len(args))
			failures := 0

			for i := range scheduled {
				result := resolveScheduled(ctx, rt, &scheduled[i], !noWait, timeout)
				if result.Status != string(task.StatusSucceeded) &&
					result.Status != statusIgnored &&
					(!noWait || result.Status != string(task.StatusPending)) {
					failures++
				}

				if !opts.json {
					line := fmt.Sprintf("%s: %s", result.File, result.Status)
					if result.Error != "" {
						line += " (" + result.Error + ")"
					}
					_, _ = fmt.Fprintln(cmd.OutOrStdout(), line)
				}

				results = append(results, result)
			}

			if opts.json {
				if err := printJSON(cmd.OutOrStdout(), results); err != nil {
					return err
				}
			}

			if failures > 0 {
				return errors.Errorf("%d of %d file(s) failed to index", failures, len(args))
			}

			return nil
		},
	}

	flags := cmd.Flags()
	flags.StringVarP(&collection, "collection", "c", "default", "target collection (label or ID, created if missing)")
	flags.StringArrayVar(&metaPairs, "meta", nil, "attach metadata to the documents (key=value, repeatable)")
	flags.StringVar(&baseDir, "base-dir", "", "store sources relative to this directory instead of their absolute path")
	flags.BoolVar(&noWait, "no-wait", false, "schedule indexing without waiting for completion")
	flags.BoolVar(&noIgnore, "no-ignore", false, "index files even if they match a .amoxtlignore rule")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to wait per file (0 = no timeout)")

	return cmd
}

// scheduledFile carries the outcome of scheduling a single file for indexing:
// either an early failure (taskID empty, result already terminal) or a queued
// task to wait on. It is produced by scheduleFile and consumed by
// resolveScheduled.
type scheduledFile struct {
	result  addResult
	taskID  task.ID
	started time.Time
}

// statusIgnored marks a file skipped because it matched a .amoxtlignore rule.
// It is a terminal, non-failing status: excluded from the failure tally.
const statusIgnored = "ignored"

// ignoredFile builds a terminal scheduledFile for a path excluded by a
// .amoxtlignore rule. Like an early scheduling failure it carries no taskID, so
// resolveScheduled passes it through unchanged; source is the matching
// .amoxtlignore, surfaced to the user as the result "error" line.
func ignoredFile(path, source string) scheduledFile {
	return scheduledFile{
		result: addResult{
			File:   path,
			Status: statusIgnored,
			Error:  source,
		},
	}
}

// scheduleFile validates a path and schedules its indexing task without waiting
// for completion, so a batch of files can be indexed concurrently by the task
// runner. IndexFile copies the file synchronously before returning, so the
// handle can be closed here even though indexing continues asynchronously.
// sources derives the stored source URL from the file's absolute path.
func scheduleFile(cmd *cobra.Command, rt *runtime.Runtime, collID model.CollectionID, path string, supported []string, sources *sourceMapper, metadata map[string]any) scheduledFile {
	sf := scheduledFile{started: time.Now(), result: addResult{File: path}}

	fail := func(err error) scheduledFile {
		sf.result.Status = string(task.StatusFailed)
		sf.result.Error = err.Error()
		sf.result.Duration = float64(time.Since(sf.started).Milliseconds())

		return sf
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return fail(err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return fail(err)
	}
	if info.IsDir() {
		return fail(errors.New("is a directory"))
	}

	ext := strings.ToLower(filepath.Ext(abs))
	if !slices.Contains(supported, ext) {
		return fail(errors.Errorf("unsupported file type %q (supported: %s)", ext, strings.Join(supported, ", ")))
	}

	file, err := os.Open(abs)
	if err != nil {
		return fail(err)
	}
	defer file.Close()

	source, err := sources.Source(abs)
	if err != nil {
		return fail(err)
	}

	// mtime+size based ETag: re-adding an unchanged file is a cheap no-op.
	etag := fmt.Sprintf("%x-%x", info.ModTime().Unix(), info.Size())

	indexOpts := []amoxtli.IndexFileOption{
		amoxtli.WithIndexFileSource(source),
		amoxtli.WithIndexFileETag(etag),
	}
	if metadata != nil {
		indexOpts = append(indexOpts, amoxtli.WithIndexFileMetadata(metadata))
	}

	taskID, err := rt.Codex.IndexFile(cmd.Context(), collID, filepath.Base(abs), file, indexOpts...)
	if err != nil {
		return fail(err)
	}

	sf.taskID = taskID
	sf.result.TaskID = string(taskID)

	return sf
}

// resolveScheduled turns a scheduled file into its final result: an early
// scheduling failure passes through unchanged, --no-wait reports the pending
// task, otherwise it blocks on the task's completion. The elapsed duration is
// measured from scheduling time.
func resolveScheduled(ctx context.Context, rt *runtime.Runtime, sf *scheduledFile, wait bool, timeout time.Duration) addResult {
	if sf.taskID == "" {
		// Scheduling failed early; result is already terminal.
		return sf.result
	}

	if !wait {
		sf.result.Status = string(task.StatusPending)
		sf.result.Duration = float64(time.Since(sf.started).Milliseconds())

		return sf.result
	}

	state, err := waitTask(ctx, rt.Codex, sf.taskID, timeout, nil)
	if err != nil {
		sf.result.Status = string(task.StatusFailed)
		sf.result.Error = err.Error()
		sf.result.Duration = float64(time.Since(sf.started).Milliseconds())

		return sf.result
	}

	sf.result.Status = string(state.Status)
	if state.Error != nil {
		sf.result.Error = state.Error.Error()
	}
	sf.result.Duration = float64(time.Since(sf.started).Milliseconds())

	return sf.result
}
