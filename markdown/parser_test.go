package markdown

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/dustin/go-humanize"
	"github.com/pkg/errors"
)

func TestParser(t *testing.T) {
	type testCase struct {
		File              string
		ExpectedSections  int
		MaxWordPerSection int
	}

	testCases := []testCase{
		{
			File:              "testdata/test.md",
			ExpectedSections:  11,
			MaxWordPerSection: 5,
		},
		{
			File:              "testdata/le_horla.md",
			ExpectedSections:  205,
			MaxWordPerSection: 200,
		},
		{
			File:              "../index/testsuite/testdata/documents/programming_go.md",
			ExpectedSections:  4,
			MaxWordPerSection: 200,
		},
		{
			File:              "../index/testsuite/testdata/documents/programming_rust.md",
			ExpectedSections:  3,
			MaxWordPerSection: 200,
		},
		{
			File:              "../index/testsuite/testdata/documents/cooking_boeuf_bourguignon.md",
			ExpectedSections:  4,
			MaxWordPerSection: 200,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.File, func(t *testing.T) {
			data, err := os.ReadFile(tc.File)
			if err != nil {
				t.Fatalf("%+v", errors.WithStack(err))
			}

			opts := []OptionFunc{}
			if tc.MaxWordPerSection > 0 {
				opts = append(opts, WithMaxWordPerSection(tc.MaxWordPerSection))
			}

			t.Logf("Document size: %s", humanize.Bytes(uint64(len(data))))

			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			t.Logf("Total allocated heap before parsing: %s", humanize.Bytes(before.HeapAlloc))

			doc, err := Parse(data, opts...)
			if err != nil {
				t.Fatalf("%+v", errors.WithStack(err))
			}

			var after runtime.MemStats
			runtime.ReadMemStats(&after)

			if e, g := tc.ExpectedSections, model.CountSections(doc); e != g {
				t.Errorf("len(doc.Sections()): expected '%d', got '%v'", e, g)
			}

			dumpDocument(t, doc)

			t.Logf("Total allocated heap after parsing: %s (diff: %s)", humanize.Bytes(after.HeapAlloc), humanize.Bytes(after.HeapAlloc-before.HeapAlloc))
		})
	}
}

// TestParseDocumentStartingWithHeading: a document whose very first line is a
// heading — a markdown file without frontmatter, so most of them — used to lose
// that heading's whole subtree, leaving one flat section holding the entire
// file. Nothing downstream can recover from that: the file is embedded as a
// single chunk, and an excerpt of it never reaches the paragraph that matched.
func TestParseDocumentStartingWithHeading(t *testing.T) {
	const source = "# Title\n\n## First\n\nfirst body\n\n## Second\n\nsecond body\n"

	doc, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	var found model.Section
	if err := model.WalkSections(doc, func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}
		if strings.HasPrefix(string(content), "## Second") {
			found = s
		}
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if found == nil {
		t.Fatalf("expected the sub-sections of the leading heading to be indexable, got %d sections in total", model.CountSections(doc))
	}

	// Lifted sections keep an ancestry one can actually walk: the pruned
	// heading must not remain in the branch of the sections it held.
	for _, id := range found.Branch()[:len(found.Branch())-1] {
		if id == found.ID() {
			continue
		}
		if _, err := findSection(doc, id); err != nil {
			t.Errorf("section %s of the branch is not reachable: %+v", id, err)
		}
	}
}

// findSection looks a section up by ID in the document tree.
func findSection(doc *Document, id model.SectionID) (model.Section, error) {
	var found model.Section

	if err := model.WalkSections(doc, func(s model.Section) error {
		if s.ID() == id {
			found = s
		}
		return nil
	}); err != nil {
		return nil, errors.WithStack(err)
	}

	if found == nil {
		return nil, errors.Errorf("no section %s in document", id)
	}

	return found, nil
}

func dumpDocument(t *testing.T, doc *Document) {
	t.Logf("Document #%s", doc.ID())
	t.Logf("├─ Total sections: %d", model.CountSections(doc))
	t.Log("├─ Sections")
	for _, s := range doc.Sections() {
		dumpSection(t, s, " ")
	}
}

func dumpSection(t *testing.T, section model.Section, indent string) {
	content, err := section.Content()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
	t.Logf("%s│", indent)
	t.Logf("%s├─ #%s (level: %v, start: %d, end: %d, characters: %d, words: %d)", indent, section.ID(), section.Level(), section.Start(), section.End(), len(content), len(text.SplitByWords(string(content))))
	t.Logf("%s├─ Branch: %v", indent, section.Branch())
	t.Logf("%s│", indent)
	t.Logf("%s│%s", indent, strings.ReplaceAll(text.MiddleOut(string(content), 10, " [...] "), "\n", " "))
	t.Logf("%s│", indent)
	if len(section.Sections()) > 0 {
		t.Logf("%s├─ Sections", indent)
		for _, ss := range section.Sections() {
			dumpSection(t, ss, indent+" ")
		}
	}
}
