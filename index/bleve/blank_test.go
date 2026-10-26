package bleve

import (
	"context"
	"net/url"
	"testing"

	"github.com/bornholm/amoxtli/model"
)

type blankDocument struct {
	model.Document
}

func (blankDocument) Source() *url.URL {
	return &url.URL{Scheme: "file", Path: "/test/blank.md"}
}

func (blankDocument) Collections() []model.Collection {
	return nil
}

type blankSection struct {
	model.Section
}

func (blankSection) Document() model.Document {
	return blankDocument{}
}

func (blankSection) Content() ([]byte, error) {
	return []byte("\n"), nil
}

// TestIndexSkipsBlankSections: markdown.Trim renders an empty chunk as "\n", so
// a section without text does not come with an empty content. Checking the
// length alone let bleve index it, while sqlitevec already trimmed it first.
func TestIndexSkipsBlankSections(t *testing.T) {
	var idx Index

	id, resource, err := idx.getIndexableResource(context.Background(), blankSection{})
	if err != nil {
		t.Fatalf("getIndexableResource() error: %+v", err)
	}

	if id != "" || resource != nil {
		t.Errorf("expected a blank section to be skipped, got id %q and resource %v", id, resource)
	}
}
