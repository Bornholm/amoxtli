// Package cli implements the amoxtli command line interface: a thin layer
// over the library that manages a per-project workspace (.amoxtli directory)
// holding the configuration and the indexed data.
package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bornholm/amoxtli/internal/build"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/spf13/cobra"
)

// rootOptions carries the global flags shared by every subcommand.
type rootOptions struct {
	json    bool
	dir     string
	config  string
	verbose bool
}

// workspace locates the workspace from the global flags: --config bypasses
// discovery entirely, -C changes the starting directory.
func (o *rootOptions) workspace() (*workspace.Workspace, error) {
	if o.config != "" {
		abs, err := filepath.Abs(o.config)
		if err != nil {
			return nil, err
		}

		return workspace.New(filepath.Dir(abs)), nil
	}

	start := o.dir
	if start == "" {
		start = "."
	}

	return workspace.Discover(start)
}

// loadConfig locates the workspace and loads its configuration.
func (o *rootOptions) loadConfig() (*workspace.Workspace, *config.Config, error) {
	ws, err := o.workspace()
	if err != nil {
		return nil, nil, err
	}

	cfg, err := config.Load(ws.ConfigPath())
	if err != nil {
		return nil, nil, err
	}

	return ws, cfg, nil
}

// NewRootCommand builds the amoxtli command tree.
func NewRootCommand() *cobra.Command {
	opts := &rootOptions{}

	cmd := &cobra.Command{
		Use:           "amoxtli",
		Short:         "Index and search local documents",
		Long:          "amoxtli indexes local files into a per-project hybrid search index\n(full-text + optional semantic vectors) stored in a .amoxtli directory.",
		Version:       build.LongVersion,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			// Logs go to stderr: stdout is reserved for command output and,
			// for the mcp command, for the protocol itself.
			level := slog.LevelWarn
			if opts.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level})))
		},
	}

	flags := cmd.PersistentFlags()
	flags.BoolVar(&opts.json, "json", false, "output machine-readable JSON")
	flags.StringVarP(&opts.dir, "chdir", "C", "", "start workspace discovery from this directory")
	flags.StringVar(&opts.config, "config", "", "path to a config.yaml (bypass workspace discovery)")
	flags.BoolVarP(&opts.verbose, "verbose", "v", false, "enable debug logging on stderr")

	cmd.AddCommand(
		newInitCommand(opts),
		newAddCommand(opts),
		newSearchCommand(opts),
		newDocCommand(opts),
		newCollectionCommand(opts),
		newTaskCommand(opts),
		newReindexCommand(opts),
		newCleanupCommand(opts),
		newBackupCommand(opts),
		newRestoreCommand(opts),
		newMCPCommand(opts),
	)

	return cmd
}

// Execute runs the CLI and exits the process on error.
func Execute() {
	cmd := NewRootCommand()
	if err := cmd.Execute(); err != nil {
		if cmd.PersistentFlags().Changed("verbose") {
			fmt.Fprintf(os.Stderr, "error: %+v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}
