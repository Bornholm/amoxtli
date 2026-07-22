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

const sourceFile = `package greeting

// ParseGreetingMessage parses a greeting message and returns its recipient.
func ParseGreetingMessage(message string) string {
	return message
}
`

// setupWorkspace creates a bleve-only workspace with one indexed markdown
// document and one indexed source file.
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

	codeSource := &url.URL{Scheme: "mem", Path: "/greeting.go"}
	taskID, err = rt.Codex.IndexFile(ctx, collID, "greeting.go", strings.NewReader(sourceFile), amoxtli.WithIndexFileSource(codeSource))
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
	tools := map[string]*mcp.Tool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}
	for _, name := range []string{"search", "fetch_sections", "list_collections", "list_documents"} {
		if tools[name] == nil {
			t.Errorf("tool %q not advertised", name)
		}
	}

	// Search depth is a workspace configuration concern: the agent must not be
	// offered a knob to opt into iterative retrieval per call.
	schema, err := json.Marshal(tools["search"].InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(schema), "deep") {
		t.Errorf("search must not advertise a deep parameter, got schema %s", schema)
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

	// a type=code filter must return the source file only.
	coded, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "parse greeting message", "filters": []string{"type=code"}},
	})
	if err != nil {
		t.Fatalf("call filtered search: %+v", err)
	}
	if coded.IsError {
		t.Fatalf("filtered search returned an error: %+v", coded.Content)
	}

	decodeStructured(t, coded.StructuredContent, &out)
	if len(out.Results) == 0 || !strings.Contains(out.Results[0].Source, "greeting.go") {
		t.Fatalf("expected a type=code hit on greeting.go, got: %+v", out)
	}

	// The metadata driving the filter must come back with the result, so the
	// agent can discover the filterable keys instead of guessing them.
	if got := out.Results[0].Metadata["type"]; got != "code" {
		t.Errorf("expected type=code metadata on the result, got %+v", out.Results[0].Metadata)
	}
	if got := out.Results[0].Metadata["language"]; got != "go" {
		t.Errorf("expected language=go metadata on the result, got %+v", out.Results[0].Metadata)
	}

	// a "!type" filter selects the documents carrying no type: the markdown
	// documentation, never the source file. "type!=code" would not do, since
	// every operator but !key requires the key to be present.
	docsOnly, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "concurrency goroutines", "filters": []string{"!type"}},
	})
	if err != nil {
		t.Fatalf("call filtered search: %+v", err)
	}

	decodeStructured(t, docsOnly.StructuredContent, &out)
	if len(out.Results) == 0 || !strings.Contains(out.Results[0].Source, "go-intro") {
		t.Fatalf("expected the untyped markdown to survive the !type filter, got: %+v", out)
	}
	for _, result := range out.Results {
		if strings.Contains(result.Source, "greeting.go") {
			t.Errorf("!type returned the code file: %+v", result)
		}
	}

	// an invalid filter expression must be reported as a tool error.
	invalid, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "x", "filters": []string{"no-operator"}},
	})
	if err != nil {
		t.Fatalf("call invalid filtered search: %+v", err)
	}
	if !invalid.IsError {
		t.Error("expected an invalid filter expression to be an error")
	}

	// list_documents must expose both documents.
	listed, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_documents"})
	if err != nil {
		t.Fatalf("call list_documents: %+v", err)
	}

	var docs listDocumentsOutput
	decodeStructured(t, listed.StructuredContent, &docs)
	if docs.Total != 2 {
		t.Errorf("expected two documents, got %d", docs.Total)
	}

	// The listing is header-only, but metadata is part of the header.
	var code *documentHeader
	for i, doc := range docs.Documents {
		if strings.Contains(doc.Source, "greeting.go") {
			code = &docs.Documents[i]
		}
	}
	if code == nil {
		t.Fatalf("greeting.go not listed: %+v", docs.Documents)
	}
	if got := code.Metadata["language"]; got != "go" {
		t.Errorf("expected language=go metadata on the listed document, got %+v", code.Metadata)
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
