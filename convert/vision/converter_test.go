package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	amoxtlivision "github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

// 1x1 transparent PNG.
var pngPixel = mustDecodeBase64("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")

const expectedMarkdown = `---
type: image
---

# Diagramme d'architecture

Un schéma reliant le convertisseur à l'index.

## Texte visible

ingestion
index
`

func TestConverterGolden(t *testing.T) {
	describer := &stubDescriber{
		desc: &amoxtlivision.Description{
			Title:       "Diagramme d'architecture",
			Description: "Un schéma reliant le convertisseur à l'index.",
			Text:        "ingestion\nindex",
		},
	}

	got := convertImage(t, NewConverter(describer), "schema.png", pngPixel)

	if expectedMarkdown != got {
		t.Errorf("markdown:\nexpected:\n%s\ngot:\n%s", expectedMarkdown, got)
	}

	if e, g := "image/png", describer.mimeType; e != g {
		t.Errorf("describer mime type: expected %q, got %q", e, g)
	}

	if !bytes.Equal(pngPixel, describer.data) {
		t.Error("describer data: expected the raw image bytes")
	}
}

func TestConverterOmitsEmptySections(t *testing.T) {
	describer := &stubDescriber{
		desc: &amoxtlivision.Description{Description: "Une capture d'écran."},
	}

	got := convertImage(t, NewConverter(describer), "captures/2026-07-28.jpg", pngPixel)

	// No title from the model: the file name (without extension) takes over.
	if !strings.Contains(got, "# 2026-07-28\n") {
		t.Errorf("markdown: expected the file name as fallback title, got:\n%s", got)
	}

	if strings.Contains(got, "Texte visible") {
		t.Errorf("markdown: expected no visible-text section, got:\n%s", got)
	}
}

func TestConverterMarkdownIsParsedWithImageMetadata(t *testing.T) {
	describer := &stubDescriber{
		desc: &amoxtlivision.Description{
			Title:       "Diagramme d'architecture",
			Description: "Un schéma reliant le convertisseur à l'index.",
		},
	}

	data := convertImage(t, NewConverter(describer), "schema.png", pngPixel)

	doc, err := markdown.Parse([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	metadata := model.Metadata(doc)
	if e, g := "image", metadata["type"]; e != g {
		t.Errorf("metadata[\"type\"]: expected %q, got %v", e, g)
	}
}

func TestConverterRejectsImageAboveSourceLimitWithoutCallingTheModel(t *testing.T) {
	describer := &stubDescriber{
		desc:           &amoxtlivision.Description{Description: "..."},
		maxBytes:       8,
		maxSourceBytes: 8,
	}

	_, err := NewConverter(describer).Convert(context.Background(), "schema.png", bytes.NewReader(pngPixel))
	if !errors.Is(err, amoxtlivision.ErrImageTooLarge) {
		t.Fatalf("expected ErrImageTooLarge, got %+v", err)
	}

	if describer.calls != 0 {
		t.Errorf("describer.calls: expected 0, got %d", describer.calls)
	}
}

func TestConverterHandsOversizedImageToTheDescriber(t *testing.T) {
	// Between the model limit and the source limit the converter reads the file
	// whole: shrinking it is the describer's job, not a reason to fail the
	// indexation of the image.
	describer := &stubDescriber{
		desc:     &amoxtlivision.Description{Description: "..."},
		maxBytes: 8,
	}

	if _, err := NewConverter(describer).Convert(context.Background(), "schema.png", bytes.NewReader(pngPixel)); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if describer.calls != 1 {
		t.Fatalf("describer.calls: expected 1, got %d", describer.calls)
	}

	if !bytes.Equal(pngPixel, describer.data) {
		t.Error("describer.data: expected the whole image to be handed over")
	}
}

func TestConverterRoutedByExtension(t *testing.T) {
	describer := &stubDescriber{desc: &amoxtlivision.Description{Description: "..."}}

	converter := NewConverter(describer, ".png")

	if e, g := []string{".png"}, converter.SupportedExtensions(); len(g) != 1 || e[0] != g[0] {
		t.Errorf("SupportedExtensions(): expected %v, got %v", e, g)
	}

	// Directly: an unsupported extension is refused.
	_, err := converter.Convert(context.Background(), "notes.txt", bytes.NewReader(pngPixel))
	if !errors.Is(err, convert.ErrNotSupported) {
		t.Fatalf("expected convert.ErrNotSupported, got %+v", err)
	}

	// Through the router: same outcome, and no image is read.
	routed := convert.NewRouted(converter)

	if _, err := routed.Convert(context.Background(), "notes.txt", bytes.NewReader(pngPixel)); !errors.Is(err, convert.ErrNotSupported) {
		t.Fatalf("expected convert.ErrNotSupported, got %+v", err)
	}

	if _, err := routed.Convert(context.Background(), "schema.png", bytes.NewReader(pngPixel)); err != nil {
		t.Fatalf("unexpected error on a supported extension: %+v", err)
	}

	// convert.Routed matches extensions case-sensitively, but a converter
	// reached directly accepts an uppercase extension.
	if _, err := converter.Convert(context.Background(), "schema.PNG", bytes.NewReader(pngPixel)); err != nil {
		t.Fatalf("unexpected error on an uppercase extension: %+v", err)
	}
}

func convertImage(t *testing.T, converter *Converter, filename string, data []byte) string {
	t.Helper()

	readCloser, err := converter.Convert(context.Background(), filename, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	defer readCloser.Close()

	converted, err := io.ReadAll(readCloser)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	return string(converted)
}

// stubDescriber records what it was handed and replays a canned description.
type stubDescriber struct {
	desc           *amoxtlivision.Description
	err            error
	maxBytes       int64
	maxSourceBytes int64

	calls    int
	mimeType string
	data     []byte
}

func (d *stubDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*amoxtlivision.Description, error) {
	d.calls++
	d.mimeType = mimeType
	d.data = data

	if d.err != nil {
		return nil, d.err
	}

	return d.desc, nil
}

func (d *stubDescriber) MaxImageBytes() int64 {
	return d.maxBytes
}

func (d *stubDescriber) MaxSourceBytes() int64 {
	return d.maxSourceBytes
}

var _ amoxtlivision.Describer = &stubDescriber{}

func mustDecodeBase64(data string) []byte {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		panic(err)
	}

	return decoded
}
