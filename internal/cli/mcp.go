package cli

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/bornholm/amoxtli/internal/cli/mcpserver"
	"github.com/spf13/cobra"
)

// newMCPCommand builds the "mcp" command tree. It exposes two transports:
// "stdio" (one process per client, spawned by the MCP client) and "http" (a
// single long-lived process serving many concurrent client sessions, for a
// shared deployment). Invoking "mcp" with no subcommand defaults to stdio for
// backward compatibility.
func newMCPCommand(opts *rootOptions) *cobra.Command {
	stdio := newMCPStdioCommand(opts)

	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the workspace over the Model Context Protocol",
		Long:  "Exposes local search to an LLM agent over MCP. The \"stdio\" transport\nspeaks the protocol on stdin/stdout (one process per client); the \"http\"\ntransport serves many concurrent client sessions from a single process.\nWith no subcommand, stdio is used.",
		Args:  cobra.NoArgs,
		RunE:  stdio.RunE,
	}

	cmd.AddCommand(stdio, newMCPHTTPCommand(opts))

	return cmd
}

func newMCPStdioCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "stdio",
		Short: "Serve MCP over stdio (one process per client)",
		Long:  "Speaks the protocol on stdin/stdout. All diagnostics go to stderr;\nstdout carries the protocol only.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The transport owns stdio; make sure signals stop the server cleanly.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return runMCPServer(ctx, opts, func(ctx context.Context, server *mcpserver.Server) error {
				return server.RunStdio(ctx)
			})
		},
	}
}

func newMCPHTTPCommand(opts *rootOptions) *cobra.Command {
	var addr string

	cmd := &cobra.Command{
		Use:   "http",
		Short: "Serve MCP over HTTP (shared, multi-session)",
		Long:  "Serves the streamable HTTP transport from a single long-lived process,\nhandling many concurrent client sessions. Combine with a postgres store and\nindex (index.driver: postgres) to run several instances against one database.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return runMCPServer(ctx, opts, func(ctx context.Context, server *mcpserver.Server) error {
				return server.RunHTTP(ctx, addr)
			})
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "address to listen on")

	return cmd
}

// runMCPServer loads the workspace, opens the server and runs it with the given
// transport runner, ensuring the runtime is released on exit.
func runMCPServer(ctx context.Context, opts *rootOptions, run func(context.Context, *mcpserver.Server) error) error {
	ws, cfg, err := opts.loadConfig()
	if err != nil {
		return err
	}

	server, err := mcpserver.New(ctx, ws, cfg)
	if err != nil {
		return err
	}
	defer server.Close()

	return run(ctx, server)
}
