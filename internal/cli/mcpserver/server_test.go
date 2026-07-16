package mcpserver

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/bornholm/amoxtli/task"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const doc = `# Go Programming Language

Go is a statically typed, compiled language designed at Google.

## Concurrency

Go's concurrency model is built around goroutines and channels.
`

// setupWorkspace creates a bleve-only workspace with one indexed document.
func setupWorkspace(t *testing.T) (*workspace.Workspace, *config.Config) {
	t.Helper()

	root := t.TempDir()
	ws := workspace.New(filepath.Join(root, workspace.DirName))
	if err := os.MkdirAll(ws.DataDir(), 0750); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()

	ctx := context.Background()

	rt, err := runtime.Open(ctx, ws, cfg, "test-setup")
	if err != nil {
		t.Fatalf("could not open runtime: %+v", err)
	}
	defer rt.Close()

	collID, err := rt.Codex.CreateCollection(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	source := &url.URL{Scheme: "mem", Path: "/go-intro"}
	taskID, err := rt.Codex.IndexFile(ctx, collID, "go-intro.md", strings.NewReader(doc), amoxtli.WithIndexFileSource(source))
	if err != nil {
		t.Fatal(err)
	}

	waitTask(t, ctx, rt.Codex, taskID)

	return ws, cfg
}

func waitTask(t *testing.T, ctx context.Context, codex *amoxtli.Codex, id task.ID) {
	t.Helper()

	deadline := time.Now().Add(time.Minute)
	for {
		state, err := codex.TaskState(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if state.Status == task.StatusSucceeded {
			return
		}
		if state.Status == task.StatusFailed {
			t.Fatalf("indexing failed: %v", state.Error)
		}
		if time.Now().After(deadline) {
			t.Fatalf("indexing timed out (status %s)", state.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMCPSearch(t *testing.T) {
	ws, cfg := setupWorkspace(t)

	ctx := context.Background()

	server, err := New(ctx, ws, cfg)
	if err != nil {
		t.Fatalf("could not create MCP server: %+v", err)
	}
	defer server.Close()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	if _, err := server.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %+v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %+v", err)
	}
	defer session.Close()

	// The four read-only tools must be advertised.
	tools := map[string]bool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = true
	}
	for _, name := range []string{"search", "fetch_sections", "list_collections", "list_documents"} {
		if !tools[name] {
			t.Errorf("tool %q not advertised", name)
		}
	}

	// search must find the document and inline its section contents.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "concurrency goroutines"},
	})
	if err != nil {
		t.Fatalf("call search: %+v", err)
	}
	if result.IsError {
		t.Fatalf("search returned an error: %+v", result.Content)
	}

	var out searchOutput
	decodeStructured(t, result.StructuredContent, &out)

	if len(out.Results) == 0 || !strings.Contains(out.Results[0].Source, "go-intro") {
		t.Fatalf("unexpected search results: %+v", out)
	}
	if len(out.Results[0].Sections) == 0 || out.Results[0].Sections[0].Content == "" {
		t.Errorf("expected inline section content, got %+v", out.Results[0])
	}

	// deep search must be refused without a chat model.
	deep, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "x", "deep": true},
	})
	if err != nil {
		t.Fatalf("call deep search: %+v", err)
	}
	if !deep.IsError {
		t.Error("expected deep search without a chat model to be an error")
	}

	// list_documents must expose the single document.
	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_documents"})
	if err != nil {
		t.Fatalf("call list_documents: %+v", err)
	}

	var docs listDocumentsOutput
	decodeStructured(t, listed.StructuredContent, &docs)
	if docs.Total != 1 {
		t.Errorf("expected one document, got %d", docs.Total)
	}
}

func decodeStructured(t *testing.T, structured any, target any) {
	t.Helper()

	raw, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatalf("could not decode structured content %s: %v", raw, err)
	}
}
