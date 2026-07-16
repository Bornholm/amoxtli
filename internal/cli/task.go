package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/task"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

func newTaskCommand(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "task",
		Aliases: []string{"tasks"},
		Short:   "Inspect indexing tasks",
	}

	cmd.AddCommand(
		newTaskListCommand(opts),
		newTaskShowCommand(opts),
		newTaskCancelCommand(opts),
	)

	return cmd
}

func newTaskListCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List tasks known to the runner",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				headers, err := rt.Codex.ListTasks(cmd.Context())
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					return printJSON(cmd.OutOrStdout(), headers)
				}

				tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tTYPE\tSTATUS\tSCHEDULED")
				for _, h := range headers {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", h.ID, h.Type, h.Status, h.ScheduledAt.Format("2006-01-02 15:04:05"))
				}
				tw.Flush()

				return nil
			})
		},
	}
}

func newTaskShowCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "show <id>",
		Short: "Show a task's state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				state, err := rt.Codex.TaskState(cmd.Context(), task.ID(args[0]))
				if err != nil {
					return errors.WithStack(err)
				}

				if opts.json {
					payload := map[string]any{
						"id":       string(state.ID),
						"type":     string(state.Type),
						"status":   string(state.Status),
						"progress": state.Progress,
						"message":  state.Message,
					}
					if state.Error != nil {
						payload["error"] = state.Error.Error()
					}
					return printJSON(cmd.OutOrStdout(), payload)
				}

				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "ID:       %s\n", state.ID)
				fmt.Fprintf(out, "Type:     %s\n", state.Type)
				fmt.Fprintf(out, "Status:   %s\n", state.Status)
				fmt.Fprintf(out, "Progress: %.0f%%\n", state.Progress*100)
				if state.Message != "" {
					fmt.Fprintf(out, "Message:  %s\n", state.Message)
				}
				if state.Error != nil {
					fmt.Fprintf(out, "Error:    %s\n", state.Error)
				}

				return nil
			})
		},
	}
}

func newTaskCancelCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <id>",
		Short: "Cancel a scheduled or running task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withRuntime(opts, cmd, func(rt *runtime.Runtime) error {
				if err := rt.Codex.CancelTask(cmd.Context(), task.ID(args[0])); err != nil {
					return errors.WithStack(err)
				}

				fmt.Fprintf(cmd.OutOrStdout(), "Cancelled task %s\n", args[0])

				return nil
			})
		},
	}
}
