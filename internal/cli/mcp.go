package cli

import (
	"os/signal"
	"syscall"

	"github.com/bornholm/amoxtli/internal/cli/mcpserver"
	"github.com/spf13/cobra"
)

func newMCPCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "mcp",
		Short: "Serve the workspace over the Model Context Protocol (stdio)",
		Long:  "Exposes local search to an LLM agent over MCP on stdin/stdout. All\ndiagnostics go to stderr; stdout carries the protocol only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The transport owns stdio; make sure signals stop the server cleanly.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			ws, cfg, err := opts.loadConfig()
			if err != nil {
				return err
			}

			server, err := mcpserver.New(ctx, ws, cfg)
			if err != nil {
				return err
			}
			defer server.Close()

			return server.Run(ctx)
		},
	}
}
