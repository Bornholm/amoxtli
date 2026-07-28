package imagetext

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/vision"
)

func TestInlineLocalImages(t *testing.T) {
	dir := t.TempDir()
	data := imageBytes(t, 64, 64)

	if err := os.MkdirAll(filepath.Join(dir, "media"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media", "image1.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	const document = `# Titre

![Schéma](media/image1.png)

Du texte.
`

	inlined := inline(t, []byte(document), dir, 0)

	expected := "![Schéma](data:image/png;base64," + base64.StdEncoding.EncodeToString(data) + ")"

	if !strings.Contains(string(inlined), expected) {
		t.Errorf("inlined document: expected the data URI, got:\n%s", inlined)
	}

	// Everything else is preserved byte for byte.
	if !strings.HasPrefix(string(inlined), "# Titre\n\n") || !strings.HasSuffix(string(inlined), "\n\nDu texte.\n") {
		t.Errorf("inlined document: expected the surrounding markdown untouched, got:\n%s", inlined)
	}
}

func TestInlineLocalImagesIsEnrichable(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "image1.png"), 128, 128)

	// The whole point of phase 3: what pandoc produces must be describable by
	// the enrichment without it knowing anything about pandoc.
	inlined := inline(t, []byte("![Schéma](media/image1.png)\n"), dir, 0)

	describer := &stubDescriber{desc: &vision.Description{Description: "Un schéma extrait du document"}}

	enriched := enrich(t, inlined, WithDescriber(describer))

	if describer.calls != 1 {
		t.Fatalf("describer.calls: expected 1, got %d", describer.calls)
	}

	if !strings.Contains(string(enriched), "Un schéma extrait du document") {
		t.Errorf("enriched document: expected the description, got:\n%s", enriched)
	}
}

func TestInlineLocalImagesKeepsRefusedDestinations(t *testing.T) {
	dir := t.TempDir()
	writeImage(t, filepath.Join(dir, "media", "image1.png"), 64, 64)
	writeImage(t, filepath.Join(filepath.Dir(dir), "outside.png"), 64, 64)

	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		Name        string
		Destination string
		MaxBytes    int64
	}{
		{"traversal", "../outside.png", 0},
		{"absolute", "/etc/hosts.png", 0},
		{"missing", "media/absent.png", 0},
		{"not an image", "notes.txt", 0},
		{"remote", "https://example.net/schema.png", 0},
		{"too large", "media/image1.png", 8},
	}

	for _, tc := range testCases {
		t.Run(tc.Name, func(t *testing.T) {
			document := []byte("![Image](" + tc.Destination + ")\n")

			inlined := inline(t, document, dir, tc.MaxBytes)

			if !bytes.Equal(document, inlined) {
				t.Errorf("inlined document: expected the link kept as is, got:\n%s", inlined)
			}
		})
	}
}

func TestInlineLocalImagesLeavesDataURIsAlone(t *testing.T) {
	document := []byte("![Schéma](data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes(t, 64, 64)) + ")\n")

	inlined := inline(t, document, t.TempDir(), 0)

	if !bytes.Equal(document, inlined) {
		t.Error("inlined document: expected an already inline image to be untouched")
	}
}

func TestInlineLocalImagesMultipleOccurrences(t *testing.T) {
	dir := t.TempDir()
	first := imageBytes(t, 64, 64)
	second := imageBytes(t, 64, 96)

	if err := os.MkdirAll(filepath.Join(dir, "media"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media", "a.png"), first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "media", "b.png"), second, 0o600); err != nil {
		t.Fatal(err)
	}

	// Two images in the same paragraph, one of them repeated: every occurrence
	// must be rewritten, and each with its own bytes.
	document := []byte("![A](media/a.png) ![B](media/b.png) ![A encore](media/a.png)\n")

	inlined := string(inline(t, document, dir, 0))

	firstURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(first)
	secondURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString(second)

	if e, g := 2, strings.Count(inlined, firstURI); e != g {
		t.Errorf("occurrences of the first image: expected %d, got %d", e, g)
	}

	if e, g := 1, strings.Count(inlined, secondURI); e != g {
		t.Errorf("occurrences of the second image: expected %d, got %d", e, g)
	}

	if strings.Contains(inlined, "media/") {
		t.Errorf("inlined document: expected no local path left, got:\n%s", inlined)
	}
}

func TestInlineLocalImagesKeepsTitles(t *testing.T) {
	dir := t.TempDir()
	data := imageBytes(t, 64, 64)

	if err := os.WriteFile(filepath.Join(dir, "a.png"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	inlined := string(inline(t, []byte(`![A](a.png "Un titre")`+"\n"), dir, 0))

	if !strings.Contains(inlined, `;base64,`+base64.StdEncoding.EncodeToString(data)+` "Un titre")`) {
		t.Errorf("inlined document: expected the link title preserved, got:\n%s", inlined)
	}
}

func TestInlineLocalImagesWithoutDirIsANoOp(t *testing.T) {
	document := []byte("![A](a.png)\n")

	inlined := inline(t, document, "", 0)

	if !bytes.Equal(document, inlined) {
		t.Error("inlined document: expected it untouched without a directory")
	}
}

func inline(t *testing.T, data []byte, dir string, maxBytes int64) []byte {
	t.Helper()

	inlined, err := InlineLocalImages(data, dir, maxBytes)
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	return inlined
}
