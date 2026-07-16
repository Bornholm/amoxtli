package cli

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
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
		noWait     bool
		timeout    time.Duration
	)

	cmd := &cobra.Command{
		Use:   "add <file>...",
		Short: "Index local files into the workspace",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			metadata, err := parseMetadata(metaPairs)
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

			results := make([]addResult, 0, len(args))
			failures := 0

			for _, path := range args {
				result := addFile(cmd, rt, collID, path, supported, metadata, !noWait, timeout)
				if result.Status != string(task.StatusSucceeded) && !(noWait && result.Status == string(task.StatusPending)) {
					failures++
				}

				if !opts.json {
					line := fmt.Sprintf("%s: %s", result.File, result.Status)
					if result.Error != "" {
						line += " (" + result.Error + ")"
					}
					fmt.Fprintln(cmd.OutOrStdout(), line)
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
	flags.BoolVar(&noWait, "no-wait", false, "schedule indexing without waiting for completion")
	flags.DurationVar(&timeout, "timeout", 5*time.Minute, "maximum time to wait per file (0 = no timeout)")

	return cmd
}

func addFile(cmd *cobra.Command, rt *runtime.Runtime, collID model.CollectionID, path string, supported []string, metadata map[string]any, wait bool, timeout time.Duration) addResult {
	started := time.Now()

	result := addResult{File: path}

	fail := func(err error) addResult {
		result.Status = string(task.StatusFailed)
		result.Error = err.Error()
		result.Duration = float64(time.Since(started).Milliseconds())

		return result
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

	source := &url.URL{Scheme: "file", Path: abs}
	// mtime+size based ETag: re-adding an unchanged file is a cheap no-op.
	etag := fmt.Sprintf("%x-%x", info.ModTime().Unix(), info.Size())

	indexOpts := []amoxtli.IndexFileOption{
		amoxtli.WithIndexFileSource(source),
		amoxtli.WithIndexFileETag(etag),
	}
	if metadata != nil {
		indexOpts = append(indexOpts, amoxtli.WithIndexFileMetadata(metadata))
	}

	ctx := cmd.Context()

	taskID, err := rt.Codex.IndexFile(ctx, collID, filepath.Base(abs), file, indexOpts...)
	if err != nil {
		return fail(err)
	}

	result.TaskID = string(taskID)

	if !wait {
		result.Status = string(task.StatusPending)
		result.Duration = float64(time.Since(started).Milliseconds())

		return result
	}

	state, err := waitTask(ctx, rt.Codex, taskID, timeout, nil)
	if err != nil {
		return fail(err)
	}

	result.Status = string(state.Status)
	if state.Error != nil {
		result.Error = state.Error.Error()
	}
	result.Duration = float64(time.Since(started).Milliseconds())

	return result
}
