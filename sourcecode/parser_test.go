package sourcecode

import (
	"os"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
)

func TestParser(t *testing.T) {
	type testCase struct {
		File              string
		Language          string
		ExpectedSections  int
		MaxWordPerSection int
	}

	testCases := []testCase{
		{
			File:             "testdata/sample.go",
			Language:         "go",
			ExpectedSections: 5,
		},
		{
			File:             "testdata/sample.py",
			Language:         "python",
			ExpectedSections: 6,
		},
		{
			File:             "testdata/sample.ts",
			Language:         "typescript",
			ExpectedSections: 8,
		},
		{
			File:             "testdata/sample.tsx",
			Language:         "tsx",
			ExpectedSections: 3,
		},
		{
			File:             "testdata/sample.js",
			Language:         "javascript",
			ExpectedSections: 5,
		},
		{
			File:             "testdata/sample.php",
			Language:         "php",
			ExpectedSections: 6,
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

			doc, err := Parse(tc.File, data, opts...)
			if err != nil {
				t.Fatalf("%+v", errors.WithStack(err))
			}

			if e, g := tc.ExpectedSections, model.CountSections(doc); e != g {
				t.Errorf("model.CountSections(doc): expected '%d', got '%v'", e, g)
			}

			metadata := model.Metadata(doc)

			if e, g := "code", metadata["type"]; e != g {
				t.Errorf("metadata[\"type\"]: expected '%s', got '%v'", e, g)
			}

			if e, g := tc.Language, metadata["language"]; e != g {
				t.Errorf("metadata[\"language\"]: expected '%s', got '%v'", e, g)
			}

			assertSectionInvariants(t, doc, data)

			dumpDocument(t, doc)
		})
	}
}

// assertSectionInvariants checks that every section content matches its byte
// range and that branches chain from parent to child.
func assertSectionInvariants(t *testing.T, doc *Document, data []byte) {
	err := model.WalkSections(doc, func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}

		if e, g := string(data[s.Start():s.End()]), string(content); e != g {
			t.Errorf("section #%s content does not match data[%d:%d]", s.ID(), s.Start(), s.End())
		}

		branch := s.Branch()

		if len(branch) == 0 || branch[len(branch)-1] != s.ID() {
			t.Errorf("section #%s branch %v does not end with its own id", s.ID(), branch)
		}

		if parent := s.Parent(); parent != nil {
			parentBranch := parent.Branch()

			if len(branch) != len(parentBranch)+1 {
				t.Errorf("section #%s branch %v does not extend parent branch %v", s.ID(), branch, parentBranch)
			}

			if s.Start() < parent.Start() || s.End() > parent.End() {
				t.Errorf("section #%s range [%d,%d] escapes parent range [%d,%d]", s.ID(), s.Start(), s.End(), parent.Start(), parent.End())
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}
}

func TestParserNesting(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.py")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	doc, err := Parse("testdata/sample.py", data)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	var classSection model.Section

	if err := model.WalkSections(doc, func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}

		if strings.HasPrefix(string(content), "class Server") {
			classSection = s
		}

		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if classSection == nil {
		t.Fatal("could not find the 'class Server' section")
	}

	if e, g := 2, len(classSection.Sections()); e != g {
		t.Fatalf("len(classSection.Sections()): expected '%d', got '%v'", e, g)
	}

	for _, method := range classSection.Sections() {
		if e, g := classSection.Level()+1, method.Level(); e != g {
			t.Errorf("method level: expected '%d', got '%v'", e, g)
		}
	}
}

func TestParserDocComments(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.go")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	doc, err := Parse("testdata/sample.go", data)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	found := false

	if err := model.WalkSections(doc, func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}

		if strings.HasPrefix(string(content), "// Farewell says goodbye.") && strings.Contains(string(content), "func Farewell") {
			found = true
		}

		return nil
	}); err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if !found {
		t.Error("no section starts with the 'func Farewell' doc comment")
	}
}

func TestParserForceSplit(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.go")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	doc, err := Parse("testdata/sample.go", data, WithMaxWordPerSection(5))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	baseline, err := Parse("testdata/sample.go", data)
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if base, split := model.CountSections(baseline), model.CountSections(doc); split <= base {
		t.Errorf("expected more sections with MaxWordPerSection=5 (baseline: %d, got: %d)", base, split)
	}

	assertSectionInvariants(t, doc, data)
}

func TestParserFallback(t *testing.T) {
	data, err := os.ReadFile("testdata/sample.go")
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	// Force the structural extraction to be skipped: the document must degrade
	// to a single root section instead of failing.
	doc, err := Parse("testdata/sample.go", data, WithMaxParseBytes(1))
	if err != nil {
		t.Fatalf("%+v", errors.WithStack(err))
	}

	if e, g := 1, model.CountSections(doc); e != g {
		t.Errorf("model.CountSections(doc): expected '%d', got '%v'", e, g)
	}

	root := doc.Sections()[0]

	if root.Start() != 0 || root.End() != len(data) {
		t.Errorf("root section [%d,%d] does not cover the whole file (%d bytes)", root.Start(), root.End(), len(data))
	}

	if e, g := "code", model.Metadata(doc)["type"]; e != g {
		t.Errorf("metadata[\"type\"]: expected '%s', got '%v'", e, g)
	}
}

func TestParserUnsupportedExtension(t *testing.T) {
	if _, err := Parse("file.unknown", []byte("data")); !errors.Is(err, ErrUnsupportedExtension) {
		t.Errorf("expected ErrUnsupportedExtension, got %v", err)
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
	t.Logf("%s│%s", indent, strings.ReplaceAll(text.MiddleOut(string(content), 10, " [...] "), "\n", " "))
	if len(section.Sections()) > 0 {
		t.Logf("%s├─ Sections", indent)
		for _, ss := range section.Sections() {
			dumpSection(t, ss, indent+" ")
		}
	}
}
