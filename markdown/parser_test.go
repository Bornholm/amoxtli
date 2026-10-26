package markdown

import (
	"os"
	"runtime"
	"slices"
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

// TestParseLongHeadingWithInlines: a heading longer than MaxWordPerSection and
// holding several inline nodes — a paragraph styled as a title by a docx
// converter — used to be split on one of its inlines, opening a section whose
// start lay past its end and making Parse panic on the next one.
func TestParseLongHeadingWithInlines(t *testing.T) {
	// The text before the heading keeps its section from being pruned as
	// redundant with the root one, so that it can be looked up.
	const intro = "intro\n\n"

	words := strings.Repeat("word ", 300)
	source := intro + "# " + words + "*bold* " + words + "\n\nBody.\n"

	headingStart := len(intro)
	headingEnd := strings.Index(source, "\n\nBody.")

	doc, err := Parse([]byte(source), WithMaxWordPerSection(250))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	var heading model.Section

	if err := model.WalkSections(doc, func(s model.Section) error {
		if s.Start() > s.End() {
			return errors.Errorf("section %s starts at %d, past its end %d", s.ID(), s.Start(), s.End())
		}

		// The heading is one node: no section may begin or end within it.
		if s.Start() > headingStart && s.Start() < headingEnd {
			return errors.Errorf("section %s starts at %d, within the heading [%d:%d]", s.ID(), s.Start(), headingStart, headingEnd)
		}
		if s.End() > headingStart && s.End() < headingEnd {
			return errors.Errorf("section %s ends at %d, within the heading [%d:%d]", s.ID(), s.End(), headingStart, headingEnd)
		}

		if _, err := s.Content(); err != nil {
			return errors.WithStack(err)
		}

		if s.Start() == headingStart {
			heading = s
		}

		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if heading == nil {
		dumpDocument(t, doc)
		t.Fatalf("expected a section starting with the heading at %d", headingStart)
	}

	content, err := heading.Content()
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if !strings.Contains(string(content), "bold") {
		t.Error("the section of the heading misses its emphasis")
	}

	if e, g := 601, len(text.SplitByWords(string(content))); g < e {
		t.Errorf("the section of the heading holds %d words, expected at least the %d of the heading", g, e)
	}
}

// TestParseSplitsLongBody: not splitting on the nodes a section already covers
// must leave the split on the blocks that extend it alone — a body longer than
// MaxWordPerSection still ends up in several sections.
func TestParseSplitsLongBody(t *testing.T) {
	first := strings.Repeat("first ", 300)
	second := strings.Repeat("second ", 10)
	source := "# Title\n\n" + first + "\n\n" + second + "\n"

	doc, err := Parse([]byte(source), WithMaxWordPerSection(250))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	var found model.Section
	if err := model.WalkSections(doc, func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}
		if strings.HasPrefix(string(content), "second") {
			found = s
		}
		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if found == nil {
		dumpDocument(t, doc)
		t.Fatal("expected the paragraph following the overflowing one to open a section of its own")
	}
}

// TestParseSplitBranches: the sections opened by the word-count split get their
// branch the same way as those opened by a heading, and must not share it
// either.
func TestParseSplitBranches(t *testing.T) {
	paragraph := strings.Repeat("word ", 300) + "\n\n"
	source := "intro\n\n# A\n\n## B\n\n" + strings.Repeat(paragraph, 4)

	doc, err := Parse([]byte(source), WithMaxWordPerSection(250))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	assertBranches(t, doc)
}

// TestParseSiblingBranches: the branch of a section used to be appended to its
// parent's without a copy, so that once that slice had spare capacity the next
// sibling overwrote the last ID of the previous one. A document whose first
// heading is not pruned — some text before it — kept the corrupted branches.
func TestParseSiblingBranches(t *testing.T) {
	const source = "intro\n\n# A\n\n## B\n\n### B1\n\nfirst\n\n### B2\n\nsecond\n\n### B3\n\nthird\n"

	doc, err := Parse([]byte(source))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	assertBranches(t, doc)
}

// assertBranches checks that the branch of every section is the one of its
// parent followed by its own ID.
func assertBranches(t *testing.T, doc *Document) {
	t.Helper()

	if err := model.WalkSections(doc, func(s model.Section) error {
		branch := s.Branch()

		if last := branch[len(branch)-1]; last != s.ID() {
			return errors.Errorf("branch of section %s ends with %s", s.ID(), last)
		}

		parent, ok := s.Parent().(*Section)
		if !ok || parent == nil {
			return nil
		}

		if !slices.Equal(branch[:len(branch)-1], parent.Branch()) {
			return errors.Errorf("branch of section %s is %v, want its parent's %v followed by its own ID", s.ID(), branch, parent.Branch())
		}

		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
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
