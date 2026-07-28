package markdown

import (
	"testing"

	"github.com/bornholm/amoxtli/model"
)

func TestParseHoistsFrontmatterMetadata(t *testing.T) {
	const document = `---
source: https://example.net/schema.png
type: image
language: fr
pages: 12
ratio: 1.5
draft: true
tags:
  - a
  - b
authors:
  name: Ada
---

# Diagramme

Un schéma.
`

	doc, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	metadata := model.Metadata(doc)

	expected := map[string]any{
		"type":     "image",
		"language": "fr",
		"pages":    float64(12),
		"ratio":    1.5,
		"draft":    true,
	}

	for key, want := range expected {
		got, exists := metadata[key]
		if !exists {
			t.Errorf("metadata[%q]: missing", key)
			continue
		}
		if want != got {
			t.Errorf("metadata[%q]: expected %#v, got %#v", key, want, got)
		}
	}

	// "source" stays the document source URL, it is not duplicated as metadata.
	if _, exists := metadata["source"]; exists {
		t.Error("metadata[\"source\"]: expected the source key to be excluded")
	}

	if doc.Source() == nil || doc.Source().String() != "https://example.net/schema.png" {
		t.Errorf("doc.Source(): expected the frontmatter source, got %v", doc.Source())
	}

	// Composite values have no JSON-scalar equivalent for the index backends.
	for _, key := range []string{"tags", "authors"} {
		if _, exists := metadata[key]; exists {
			t.Errorf("metadata[%q]: expected composite values to be ignored", key)
		}
	}
}

func TestParseWithoutFrontmatterHasNoMetadata(t *testing.T) {
	doc, err := Parse([]byte("# Diagramme\n\nUn schéma.\n"))
	if err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if metadata := model.Metadata(doc); len(metadata) != 0 {
		t.Errorf("metadata: expected none, got %#v", metadata)
	}
}
