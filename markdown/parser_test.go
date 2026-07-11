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
