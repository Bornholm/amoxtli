package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/internal/cli/runtime"
)

// 1x1 transparent PNG.
var testImagePNG = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

// setupImageWorkspace initializes a workspace whose image store holds one
// image, and returns its root and the hash of that image.
func setupImageWorkspace(t *testing.T) (string, string) {
	t.Helper()

	root := t.TempDir()

	mustRunCLI(t, "-C", root, "init")
	enableImageStore(t, root)

	hash := putTestImage(t, root, testImagePNG)

	return root, hash
}

// enableImageStore switches the generated config to an explicit filesystem
// image store: the workspace has no vision model, so "auto" would leave the
// store disabled.
func enableImageStore(t *testing.T, root string) {
	t.Helper()

	path := filepath.Join(root, ".amoxtli", "config.yaml")

	config, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// The generated template already carries an images block; switch its
	// backend rather than appending a duplicate key.
	updated := bytes.Replace(config, []byte("\n  store: auto\n"), []byte("\n  store: fs\n"), 1)
	if bytes.Equal(config, updated) {
		t.Fatal("could not find the images.store entry in the generated config")
	}

	if err := os.WriteFile(path, updated, 0600); err != nil {
		t.Fatal(err)
	}
}

// putTestImage stores an image through the library and returns its hash.
func putTestImage(t *testing.T, root string, data []byte) string {
	t.Helper()

	ws, cfg, err := (&rootOptions{dir: root}).loadConfig()
	if err != nil {
		t.Fatal(err)
	}

	rt, err := runtime.Open(t.Context(), ws, cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	hash, err := rt.Codex.Blobs().Put(t.Context(), "image/png", data)
	if err != nil {
		t.Fatal(err)
	}

	return string(hash)
}

func TestCLIImageList(t *testing.T) {
	root, hash := setupImageWorkspace(t)

	output := mustRunCLI(t, "-C", root, "image", "list")

	if !strings.Contains(output, hash) {
		t.Errorf("image list must show the hash, got:\n%s", output)
	}

	if !strings.Contains(output, "image/png") {
		t.Errorf("image list must show the media type, got:\n%s", output)
	}

	if !strings.Contains(output, "1 image(s)") {
		t.Errorf("image list must show a total, got:\n%s", output)
	}

	// JSON mode exposes the URI, which is what appears in section contents.
	output = mustRunCLI(t, "-C", root, "--json", "image", "list")

	var images []imageInfo
	if err := json.Unmarshal([]byte(output), &images); err != nil {
		t.Fatalf("could not parse output %q: %v", output, err)
	}

	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}

	if e, g := "amoxtli://images/"+hash, images[0].URI; e != g {
		t.Errorf("image URI: expected %q, got %q", e, g)
	}

	if e, g := int64(len(testImagePNG)), images[0].Size; e != g {
		t.Errorf("image size: expected %d, got %d", e, g)
	}
}

func TestCLIImageGet(t *testing.T) {
	root, hash := setupImageWorkspace(t)

	target := filepath.Join(t.TempDir(), "schema.png")

	// By URI.
	output := mustRunCLI(t, "-C", root, "image", "get", "amoxtli://images/"+hash, "-o", target)

	if !strings.Contains(output, "wrote "+target) {
		t.Errorf("expected a confirmation on stderr, got:\n%s", output)
	}

	written, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(testImagePNG, written) {
		t.Error("the written file must hold the stored bytes")
	}

	// By bare hash.
	second := filepath.Join(t.TempDir(), "again.png")
	mustRunCLI(t, "-C", root, "image", "get", hash, "-o", second)

	written, err = os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(testImagePNG, written) {
		t.Error("a bare hash must resolve like a URI")
	}
}

// TestCLIImageGetStdout covers the curl-like rule. The test harness captures
// output into a buffer, never a terminal, so the bytes flow — which is exactly
// the redirected/piped case.
func TestCLIImageGetStdout(t *testing.T) {
	root, hash := setupImageWorkspace(t)

	output := mustRunCLI(t, "-C", root, "image", "get", hash)

	if !strings.Contains(output, string(testImagePNG)) {
		t.Error("a non-terminal stdout must receive the raw bytes")
	}

	// "-o -" forces stdout whatever it is attached to.
	output = mustRunCLI(t, "-C", root, "image", "get", hash, "-o", "-")

	if !strings.Contains(output, string(testImagePNG)) {
		t.Error("-o - must write the raw bytes to stdout")
	}
}

func TestCLIImageErrors(t *testing.T) {
	root, hash := setupImageWorkspace(t)

	// Unknown hash.
	unknown := strings.Repeat("a", len(hash))
	if output, err := runCLI(t, "-C", root, "image", "get", unknown); err == nil {
		t.Errorf("expected an error for an unknown hash, got:\n%s", output)
	}

	// Malformed reference.
	for _, reference := range []string{"not-a-hash", "amoxtli://images/../../etc/passwd", ""} {
		if output, err := runCLI(t, "-C", root, "image", "get", reference); err == nil {
			t.Errorf("expected an error for %q, got:\n%s", reference, output)
		}
	}

	// A workspace without image store must say so, not crash.
	bare := t.TempDir()
	mustRunCLI(t, "-C", bare, "init")

	output, err := runCLI(t, "-C", bare, "image", "list")
	if err == nil {
		t.Errorf("expected an error without an image store, got:\n%s", output)
	} else if !strings.Contains(err.Error(), "images.store") {
		t.Errorf("the error must point at the configuration, got: %v", err)
	}
}

func mustDecodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}

	return decoded
}
