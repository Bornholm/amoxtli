// Package mcpserver exposes an amoxtli workspace over the Model Context
// Protocol (stdio), so an LLM agent can search the local corpus.
package mcpserver

import (
	"context"

	"github.com/bornholm/amoxtli/internal/build"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
)

// Server bundles a live Codex with an MCP server exposing it.
type Server struct {
	rt  *runtime.Runtime
	mcp *mcp.Server
}

// New opens the workspace runtime and registers the MCP tools.
func New(ctx context.Context, ws *workspace.Workspace, cfg *config.Config) (*Server, error) {
	rt, err := runtime.Open(ctx, ws, cfg, "mcp")
	if err != nil {
		return nil, err
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "amoxtli",
		Title:   "amoxtli local search",
		Version: build.Version,
	}, nil)

	srv := &Server{rt: rt, mcp: mcpServer}
	srv.registerTools(cfg.HasChat(), cfg.Retrieval.GroundingCheck)

	return srv, nil
}

// Run serves the protocol over stdio until the context is cancelled.
func (s *Server) Run(ctx context.Context) error {
	if err := s.mcp.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Close releases the underlying runtime.
func (s *Server) Close() error {
	return s.rt.Close()
}
