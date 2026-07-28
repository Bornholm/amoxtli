package imagetext

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

func TestEnrichRelativePath(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "schema.png"), 128, 128)

	const document = `# Architecture

Le schéma ci-dessous résume le pipeline.

![Schéma](media/schema.png)

## Suite

Du texte.
`

	describer := &stubDescriber{
		desc: &vision.Description{
			Title:       "Diagramme d'architecture",
			Description: "Un schéma reliant le convertisseur à l'index.",
			Text:        "ingestion",
		},
	}

	enriched := enrich(t, []byte(document), WithDescriber(describer), WithBaseDir(dir))

	const expected = `# Architecture

Le schéma ci-dessous résume le pipeline.

![Schéma](media/schema.png)

> **Image — Diagramme d'architecture** : Un schéma reliant le convertisseur à l'index. ingestion

## Suite

Du texte.
`

	if expected != string(enriched) {
		t.Errorf("enriched document:\nexpected:\n%s\ngot:\n%s", expected, enriched)
	}

	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	if e, g := "image/png", describer.mimeTypes[0]; e != g {
		t.Errorf("describer mime type: expected %q, got %q", e, g)
	}
}

func TestEnrichKeepsDescriptionInTheImageBlock(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	const document = `# Titre

## Section A

![Schéma](schema.png)

## Section B

Du texte.
`

	describer := &stubDescriber{
		desc: &vision.Description{Description: "Un schéma reliant le convertisseur à l'index."},
	}

	enriched := string(enrich(t, []byte(document), WithDescriber(describer), WithBaseDir(dir)))

	// The description must be spliced between the image and the next heading:
	// it then belongs to the same section as the image, contextualized by the
	// surrounding headings — which is what makes it useful to search and
	// grounding.
	image := strings.Index(enriched, "![Schéma]")
	description := strings.Index(enriched, "> **Image**")
	next := strings.Index(enriched, "## Section B")

	if description < image || description > next {
		t.Errorf("description offset %d: expected it between the image (%d) and the next heading (%d), got:\n%s", description, image, next, enriched)
	}

	// The result must still parse as markdown.
	if _, err := markdown.Parse([]byte(enriched)); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
}

func TestEnrichDataURL(t *testing.T) {
	data := imageBytes(t, 128, 128)

	document := "![Schéma](data:image/png;base64," + base64.StdEncoding.EncodeToString(data) + ")\n"

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma."}}

	enriched := enrich(t, []byte(document), WithDescriber(describer))

	if describer.calls != 1 {
		t.Fatalf("describer.calls: expected 1, got %d", describer.calls)
	}

	if !bytes.Equal(data, describer.payloads[0]) {
		t.Error("describer payload: expected the decoded image bytes")
	}

	if !strings.Contains(string(enriched), "> **Image** : Un schéma.") {
		t.Errorf("enriched document: expected the description, got:\n%s", enriched)
	}

	// The data URI itself is untouched; StripDataURL still removes it at render
	// time, but the description is ordinary text and survives.
	if !strings.Contains(string(enriched), "data:image/png;base64,") {
		t.Error("enriched document: expected the original data URI to be preserved")
	}
}

// TestEnrichSurvivesTrim checks the division of labour with StripDataURL: the
// inline image is still dropped from the rendered chunk (it must not pollute
// search results with base64), while the description — ordinary text — stays.
func TestEnrichSurvivesTrim(t *testing.T) {
	data := imageBytes(t, 128, 128)

	document := "# Titre\n\n![Schéma](data:image/png;base64," + base64.StdEncoding.EncodeToString(data) + ")\n"

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma indexable."}}

	enriched := enrich(t, []byte(document), WithDescriber(describer))

	doc, err := markdown.Parse(enriched)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	content, err := doc.Sections()[0].Content()
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if !strings.Contains(string(content), "Un schéma indexable.") {
		t.Errorf("rendered chunk: expected the description, got:\n%s", content)
	}

	if strings.Contains(string(content), "data:image/png;base64,") {
		t.Errorf("rendered chunk: expected the data URI to be stripped, got:\n%s", content)
	}
}

func TestEnrichMultipleImagesOffsets(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "a.png"), 128, 128)
	writeImage(t, filepath.Join(dir, "b.png"), 128, 64)

	const document = `# Titre

![A](a.png)

Entre les deux.

![B](b.png)

Fin.
`

	describer := &stubDescriber{byMime: map[string]*vision.Description{}}
	describer.desc = &vision.Description{Description: "Une image."}

	enriched := string(enrich(t, []byte(document), WithDescriber(describer), WithBaseDir(dir)))

	const expected = `# Titre

![A](a.png)

> **Image** : Une image.

Entre les deux.

![B](b.png)

> **Image** : Une image.

Fin.
`

	if expected != enriched {
		t.Errorf("enriched document:\nexpected:\n%s\ngot:\n%s", expected, enriched)
	}
}

func TestEnrichDeduplicatesIdenticalImages(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "a.png"), 128, 128)
	writeImage(t, filepath.Join(dir, "copie.png"), 128, 128)

	const document = `![A](a.png)

![Encore](a.png)

![Copie](copie.png)
`

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	enriched := string(enrich(t, []byte(document), WithDescriber(describer), WithBaseDir(dir)))

	// Same bytes under three references: one description, reinserted each time.
	if describer.calls != 1 {
		t.Errorf("describer.calls: expected 1, got %d", describer.calls)
	}

	if e, g := 3, strings.Count(enriched, "> **Image** : Une image."); e != g {
		t.Errorf("inserted descriptions: expected %d, got %d in:\n%s", e, g, enriched)
	}
}

func TestEnrichSkipsUnresolvableImages(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "small.png"), 32, 32)
	writeImage(t, filepath.Join(dir, "large.png"), 128, 128)
	writeImage(t, filepath.Join(filepath.Dir(dir), "outside.png"), 128, 128)

	testCases := []struct {
		Name        string
		Destination string
	}{
		{"traversal", "../outside.png"},
		{"absolute", "/etc/passwd.png"},
		{"missing", "absent.png"},
		{"too small", "small.png"},
		{"remote", "https://example.net/schema.png"},
		{"not an image", "notes.txt"},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			document := []byte("![Image](" + tc.Destination + ")\n")

			describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

			enriched := enrich(t, document, WithDescriber(describer), WithBaseDir(dir))

			if describer.calls != 0 {
				t.Errorf("describer.calls: expected 0, got %d", describer.calls)
			}

			if !bytes.Equal(document, enriched) {
				t.Errorf("enriched document: expected it untouched, got:\n%s", enriched)
			}
		})
	}
}

func TestEnrichWithoutBaseDirIgnoresRelativePaths(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	document := []byte("![Schéma](schema.png)\n")

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	enriched := enrich(t, document, WithDescriber(describer))

	if describer.calls != 0 {
		t.Errorf("describer.calls: expected 0 without a base directory, got %d", describer.calls)
	}

	if !bytes.Equal(document, enriched) {
		t.Errorf("enriched document: expected it untouched, got:\n%s", enriched)
	}
}

func TestEnrichCapsImagesPerDocument(t *testing.T) {
	dir := t.TempDir()

	var document strings.Builder
	for i, name := range []string{"a.png", "b.png", "c.png"} {
		// Different dimensions keep the bytes — and therefore the hashes —
		// distinct, so the cap is exercised rather than the deduplication.
		writeImage(t, filepath.Join(dir, name), 128, 128+i)
		document.WriteString("![Image](" + name + ")\n\n")
	}

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	enriched := string(enrich(t, []byte(document.String()),
		WithDescriber(describer),
		WithBaseDir(dir),
		WithMaxImagesPerDocument(2),
	))

	if describer.calls != 2 {
		t.Errorf("describer.calls: expected 2 (capped), got %d", describer.calls)
	}

	if e, g := 2, strings.Count(enriched, "> **Image**"); e != g {
		t.Errorf("inserted descriptions: expected %d, got %d", e, g)
	}
}

func TestEnrichKeepsDocumentOnDescriberError(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	document := []byte("![Schéma](schema.png)\n")

	describer := &stubDescriber{err: errors.New("provider unavailable")}

	enriched := enrich(t, document, WithDescriber(describer), WithBaseDir(dir))

	if !bytes.Equal(document, enriched) {
		t.Errorf("enriched document: expected it untouched, got:\n%s", enriched)
	}
}

func TestEnrichPropagatesCancellation(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	describer := &stubDescriber{err: context.Canceled}

	_, err := Enrich(ctx, []byte("![Schéma](schema.png)\n"),
		WithDescriber(describer),
		WithBaseDir(dir),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %+v", err)
	}
}

func TestEnrichWithoutDescriberIsANoOp(t *testing.T) {
	document := []byte("![Schéma](schema.png)\n")

	enriched, err := Enrich(context.Background(), document)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if !bytes.Equal(document, enriched) {
		t.Error("enriched document: expected it untouched")
	}
}

func TestEnrichReportsProgress(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "a.png"), 128, 128)
	writeImage(t, filepath.Join(dir, "b.png"), 128, 96)

	document := []byte("![A](a.png)\n\n![B](b.png)\n")

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	var steps [][2]int

	enrich(t, document,
		WithDescriber(describer),
		WithBaseDir(dir),
		WithProgress(func(done, total int) {
			steps = append(steps, [2]int{done, total})
		}),
	)

	if e, g := [][2]int{{1, 2}, {2, 2}}, steps; len(g) != len(e) || g[0] != e[0] || g[1] != e[1] {
		t.Errorf("progress steps: expected %v, got %v", e, g)
	}
}

func TestEnricherOverridesBaseDirPerDocument(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "schema.png"), 128, 128)

	describer := &stubDescriber{desc: &vision.Description{Description: "Une image."}}

	enricher := New(WithDescriber(describer), WithBaseDir("/nowhere"))

	enriched, err := enricher.Enrich(context.Background(), []byte("![Schéma](schema.png)\n"), dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if !strings.Contains(string(enriched), "> **Image** : Une image.") {
		t.Errorf("enriched document: expected the per-document base dir to win, got:\n%s", enriched)
	}
}

func enrich(t *testing.T, data []byte, funcs ...OptionFunc) []byte {
	t.Helper()

	enriched, err := Enrich(context.Background(), data, funcs...)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	return enriched
}

// writeImage encodes a PNG of the given dimensions at path.
func writeImage(t *testing.T, path string, width, height int) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, imageBytes(t, width, height), 0o600); err != nil {
		t.Fatal(err)
	}
}

// imageBytes encodes a deterministic PNG of the given dimensions.
func imageBytes(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := range width {
		for y := range height {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x80, A: 0xff})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// stubDescriber records what it was handed and replays a canned description.
type stubDescriber struct {
	desc   *vision.Description
	byMime map[string]*vision.Description
	err    error

	mu        sync.Mutex
	calls     int
	mimeTypes []string
	payloads  [][]byte
}

func (d *stubDescriber) Describe(ctx context.Context, mimeType string, data []byte) (*vision.Description, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls++
	d.mimeTypes = append(d.mimeTypes, mimeType)
	d.payloads = append(d.payloads, data)

	if d.err != nil {
		return nil, d.err
	}

	if desc, exists := d.byMime[mimeType]; exists {
		return desc, nil
	}

	return d.desc, nil
}

var _ vision.Describer = &stubDescriber{}
