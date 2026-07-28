package pandoc

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/markdown/imagetext"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

// TestConvertInlineMedia checks the whole phase 3 chain on a real .docx: the
// media pandoc extracts to its temporary directory must come back inside the
// markdown, since that directory is deleted when Convert returns.
func TestConvertInlineMedia(t *testing.T) {
	requirePandoc(t)

	docx := buildDocxWithImage(t)

	converted := convertFile(t, NewConverter(WithInlineMedia(0)), "document.docx", docx)

	if !strings.Contains(converted, "data:image/png;base64,") {
		t.Fatalf("converted markdown: expected an inlined image, got:\n%s", converted)
	}

	// No local path may survive: the directory it points to is already gone.
	if strings.Contains(converted, mediaDir+"/") {
		t.Errorf("converted markdown: expected no local media path, got:\n%s", converted)
	}

	if !strings.Contains(converted, "Le schéma ci-dessous") {
		t.Errorf("converted markdown: expected the document text, got:\n%s", converted)
	}

	// The point of inlining: the enrichment can now describe the image without
	// knowing anything about pandoc.
	describer := &countingDescriber{
		desc: &vision.Description{Description: "Un schéma extrait du document."},
	}

	enriched, err := imagetext.Enrich(context.Background(), []byte(converted),
		imagetext.WithDescriber(describer),
	)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	if !strings.Contains(string(enriched), "Un schéma extrait du document.") {
		t.Errorf("enriched markdown: expected the description, got:\n%s", enriched)
	}
}

// TestConvertInlineMediaTooLarge checks that a medium above the limit stays a
// link rather than being truncated or inlined.
func TestConvertInlineMediaTooLarge(t *testing.T) {
	requirePandoc(t)

	docx := buildDocxWithImage(t)

	converted := convertFile(t, NewConverter(WithInlineMedia(64)), "document.docx", docx)

	if strings.Contains(converted, "data:image/png;base64,") {
		t.Errorf("converted markdown: expected no inlined image, got:\n%s", converted)
	}

	if !strings.Contains(converted, mediaDir+"/") {
		t.Errorf("converted markdown: expected the media link kept as is, got:\n%s", converted)
	}
}

// TestConvertWithoutInlineMedia locks the default: no extraction, no data URI,
// exactly the historical behaviour.
func TestConvertWithoutInlineMedia(t *testing.T) {
	requirePandoc(t)

	docx := buildDocxWithImage(t)

	converted := convertFile(t, NewConverter(), "document.docx", docx)

	if strings.Contains(converted, "data:image/png;base64,") {
		t.Errorf("converted markdown: expected no inlined image by default, got:\n%s", converted)
	}

	if !strings.Contains(converted, "Le schéma ci-dessous") {
		t.Errorf("converted markdown: expected the document text, got:\n%s", converted)
	}
}

// TestConvertMediaLessDocument checks the non-regression on a document without
// media: enabling the extraction changes nothing to the output.
func TestConvertMediaLessDocument(t *testing.T) {
	requirePandoc(t)

	source := []byte("# Titre\n\nDu texte sans image.\n")

	withExtraction := convertFile(t, NewConverter(WithInlineMedia(0)), "document.md", source)
	without := convertFile(t, NewConverter(), "document.md", source)

	if withExtraction != without {
		t.Errorf("converted markdown:\nwith extraction:\n%s\nwithout:\n%s", withExtraction, without)
	}
}

func convertFile(t *testing.T, converter *Converter, filename string, content []byte) string {
	t.Helper()

	readCloser, err := converter.Convert(context.Background(), filename, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("unexpected error: %+v", errors.WithStack(err))
	}
	defer readCloser.Close()

	converted, err := io.ReadAll(readCloser)
	if err != nil {
		t.Fatalf("unexpected error: %+v", errors.WithStack(err))
	}

	return string(converted)
}

// buildDocxWithImage produces, with pandoc itself, a .docx embedding a PNG —
// avoiding a binary fixture in the repository.
func buildDocxWithImage(t *testing.T) []byte {
	t.Helper()

	dir := t.TempDir()

	imagePath := filepath.Join(dir, "schema.png")
	if err := os.WriteFile(imagePath, testImage(t), 0o600); err != nil {
		t.Fatal(err)
	}

	sourcePath := filepath.Join(dir, "source.md")
	source := "# Architecture\n\nLe schéma ci-dessous résume le pipeline.\n\n![Schéma](schema.png)\n"
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	docxPath := filepath.Join(dir, "document.docx")

	cmd := exec.Command("pandoc", "--from", "markdown", "--output", docxPath, sourcePath)
	cmd.Dir = dir

	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("could not build the test document: %v\n%s", err, output)
	}

	docx, err := os.ReadFile(docxPath)
	if err != nil {
		t.Fatal(err)
	}

	return docx
}

// testImage encodes a 128x128 PNG, above the minimum dimensions of the
// enrichment.
func testImage(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for x := range 128 {
		for y := range 128 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func requirePandoc(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("pandoc"); err != nil {
		t.Skip("pandoc is not available")
	}
}

// countingDescriber replays a canned description and counts calls.
type countingDescriber struct {
	desc  *vision.Description
	calls int
}

func (d *countingDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*vision.Description, error) {
	d.calls++
	return d.desc, nil
}

var _ vision.Describer = &countingDescriber{}
