// Package mcpserver exposes an amoxtli workspace over the Model Context
// Protocol (stdio), so an LLM agent can search the local corpus.
package mcpserver

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/bornholm/amoxtli/internal/build"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pkg/errors"
)

// shutdownTimeout bounds how long RunHTTP waits for in-flight requests to
// drain when the context is cancelled.
const shutdownTimeout = 10 * time.Second

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

// RunStdio serves the protocol over stdio until the context is cancelled. Use
// it when the MCP client spawns amoxtli as a subprocess (one process per
// client).
func (s *Server) RunStdio(ctx context.Context) error {
	if err := s.mcp.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// RunHTTP serves the protocol over the streamable HTTP transport on addr,
// until the context is cancelled. A single long-lived process handles many
// concurrent client sessions (the search path is read-only), which is what
// makes a shared, multi-user deployment possible.
func (s *Server) RunHTTP(ctx context.Context, addr string) error {
	handler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.mcp },
		&mcp.StreamableHTTPOptions{Logger: slog.Default()},
	)

	srv := &http.Server{Addr: addr, Handler: handler}

	// Shut the listener down when the context is cancelled (e.g. SIGINT).
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.WarnContext(ctx, "could not shut down http server cleanly", slog.Any("error", err))
		}
	}()

	slog.InfoContext(ctx, "serving MCP over HTTP", slog.String("addr", addr))

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return errors.WithStack(err)
	}

	return nil
}

// Close releases the underlying runtime.
func (s *Server) Close() error {
	return s.rt.Close()
}
