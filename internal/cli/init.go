package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

const workspaceGitignore = `data/
.env
`

func newInitCommand(opts *rootOptions) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize an amoxtli workspace in the current directory",
		Long:  "Creates a .amoxtli directory holding the workspace configuration\n(config.yaml) and the indexed data (data/).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := opts.dir
			if dir == "" {
				dir = "."
			}

			root, err := filepath.Abs(dir)
			if err != nil {
				return errors.WithStack(err)
			}

			ws := workspace.New(filepath.Join(root, workspace.DirName))

			if _, err := os.Stat(ws.Dir); err == nil && !force {
				return errors.Errorf("%s already exists (use --force to overwrite config.yaml)", ws.Dir)
			}

			if err := os.MkdirAll(ws.DataDir(), 0750); err != nil {
				return errors.WithStack(err)
			}

			files := map[string]string{
				ws.ConfigPath():                         config.Template,
				filepath.Join(ws.Dir, ".gitignore"):     workspaceGitignore,
				filepath.Join(ws.DataDir(), ".gitkeep"): "",
			}

			for path, content := range files {
				if err := os.WriteFile(path, []byte(content), 0600); err != nil {
					return errors.WithStack(err)
				}
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Initialized amoxtli workspace in %s\n", ws.Dir)
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Edit %s to configure indexes and LLM providers.\n", ws.ConfigPath())

			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config.yaml")

	return cmd
}
