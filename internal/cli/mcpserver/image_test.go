package mcpserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/runtime"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// 1x1 transparent PNG.
var testPNG = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

// setupImageWorkspace indexes a document referencing a stored image, the shape
// the vision converter produces once a blob store is configured.
func setupImageWorkspace(t *testing.T) (*workspace.Workspace, *config.Config, blob.Hash) {
	t.Helper()

	root := t.TempDir()
	ws := workspace.New(filepath.Join(root, workspace.DirName))
	if err := os.MkdirAll(ws.DataDir(), 0750); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Images.Store = config.ImageStoreFS

	ctx := context.Background()

	rt, err := runtime.Open(ctx, ws, cfg, "test-setup")
	if err != nil {
		t.Fatalf("could not open runtime: %+v", err)
	}
	defer rt.Close()

	blobs := rt.Codex.Blobs()
	if blobs == nil {
		t.Fatal("expected a blob store to be configured")
	}

	hash, err := blobs.Put(ctx, "image/png", testPNG)
	if err != nil {
		t.Fatalf("could not store image: %+v", err)
	}

	collID, err := rt.Codex.CreateCollection(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}

	document := "# Diagramme d'architecture\n\n![Diagramme](" + blob.URI(hash) + ")\n\nUn schéma reliant le convertisseur à l'index.\n"

	source := &url.URL{Scheme: "mem", Path: "/diagramme"}
	taskID, err := rt.Codex.IndexFile(ctx, collID, "diagramme.md", strings.NewReader(document), amoxtli.WithIndexFileSource(source))
	if err != nil {
		t.Fatal(err)
	}

	waitTask(t, ctx, rt.Codex, taskID)

	return ws, cfg, hash
}

func TestMCPFetchImage(t *testing.T) {
	ws, cfg, hash := setupImageWorkspace(t)

	ctx := context.Background()

	server, err := New(ctx, ws, cfg)
	if err != nil {
		t.Fatalf("could not create MCP server: %+v", err)
	}
	defer server.Close()

	session := connect(t, ctx, server)
	defer session.Close()

	// The tool is only advertised when a store backs it.
	tools := map[string]*mcp.Tool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		tools[tool.Name] = tool
	}

	if tools["fetch_image"] == nil {
		t.Fatal("tool \"fetch_image\" not advertised")
	}

	// The search tool must tell the agent what those URIs are for.
	if !strings.Contains(tools["search"].Description, "fetch_image") {
		t.Errorf("search description must mention fetch_image, got %q", tools["search"].Description)
	}

	// A search must surface the URI in the section content, otherwise the agent
	// has nothing to dereference.
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "search",
		Arguments: map[string]any{"query": "convertisseur index"},
	})
	if err != nil {
		t.Fatalf("call search: %+v", err)
	}

	var searched string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			searched += text.Text
		}
	}

	if !strings.Contains(searched, blob.URI(hash)) {
		t.Errorf("search result must carry the image URI, got %s", searched)
	}

	// fetch_image returns the image itself.
	result, err = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fetch_image",
		Arguments: map[string]any{"uri": blob.URI(hash)},
	})
	if err != nil {
		t.Fatalf("call fetch_image: %+v", err)
	}

	if result.IsError {
		t.Fatalf("fetch_image returned an error: %+v", result.Content)
	}

	if len(result.Content) != 1 {
		t.Fatalf("fetch_image: expected 1 content block, got %d", len(result.Content))
	}

	image, ok := result.Content[0].(*mcp.ImageContent)
	if !ok {
		t.Fatalf("fetch_image: expected an image block, got %T", result.Content[0])
	}

	if e, g := "image/png", image.MIMEType; e != g {
		t.Errorf("fetch_image mime type: expected %q, got %q", e, g)
	}

	if !bytes.Equal(testPNG, image.Data) {
		t.Error("fetch_image: expected the stored bytes")
	}

	// A bare hash is accepted too: agents copy back whatever they saw.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fetch_image",
		Arguments: map[string]any{"uri": string(hash)},
	}); err != nil {
		t.Errorf("call fetch_image with a bare hash: %+v", err)
	}

	// Unknown and malformed references fail cleanly.
	for _, uri := range []string{
		blob.URI(blob.ComputeHash([]byte("never stored"))),
		"amoxtli://images/../../etc/passwd",
		"",
	} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "fetch_image",
			Arguments: map[string]any{"uri": uri},
		})
		if err == nil && !result.IsError {
			t.Errorf("fetch_image(%q): expected an error", uri)
		}
	}
}

func TestMCPImageResource(t *testing.T) {
	ws, cfg, hash := setupImageWorkspace(t)

	ctx := context.Background()

	server, err := New(ctx, ws, cfg)
	if err != nil {
		t.Fatalf("could not create MCP server: %+v", err)
	}
	defer server.Close()

	session := connect(t, ctx, server)
	defer session.Close()

	found := false
	for template, err := range session.ResourceTemplates(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(template.URITemplate, "amoxtli://images/") {
			found = true
		}
	}

	if !found {
		t.Error("no resource template advertised for amoxtli://images/")
	}

	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: blob.URI(hash)})
	if err != nil {
		t.Fatalf("read resource: %+v", err)
	}

	if len(result.Contents) != 1 {
		t.Fatalf("read resource: expected 1 content, got %d", len(result.Contents))
	}

	if e, g := "image/png", result.Contents[0].MIMEType; e != g {
		t.Errorf("resource mime type: expected %q, got %q", e, g)
	}

	if !bytes.Equal(testPNG, result.Contents[0].Blob) {
		t.Error("resource: expected the stored bytes")
	}

	// An unknown hash must be a protocol-level not-found, not a silent empty.
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{
		URI: blob.URI(blob.ComputeHash([]byte("never stored"))),
	}); err == nil {
		t.Error("read resource of an unknown hash: expected an error")
	}
}

// TestMCPWithoutBlobStore locks the conditional registration: no store, no
// image tool.
func TestMCPWithoutBlobStore(t *testing.T) {
	ws, cfg := setupWorkspace(t)

	ctx := context.Background()

	server, err := New(ctx, ws, cfg)
	if err != nil {
		t.Fatalf("could not create MCP server: %+v", err)
	}
	defer server.Close()

	session := connect(t, ctx, server)
	defer session.Close()

	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "fetch_image" {
			t.Error("fetch_image must not be advertised without an image store")
		}
	}
}

// connect wires an in-memory client session to the server.
func connect(t *testing.T, ctx context.Context, server *Server) *mcp.ClientSession {
	t.Helper()

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	if _, err := server.mcp.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatalf("server connect: %+v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)

	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %+v", err)
	}

	return session
}

func mustDecodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}

	return decoded
}
