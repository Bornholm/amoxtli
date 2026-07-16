package sourcecode

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/bornholm/amoxtli/model"
	ts "github.com/odvcencio/gotreesitter"
	"github.com/pkg/errors"
)

var ErrUnsupportedExtension = errors.New("unsupported file extension")

type Options struct {
	MaxWordPerSection int
	Registry          *Registry
	// MaxParseBytes caps structural extraction: larger files are indexed as a
	// single root section (later force-split by word budget).
	MaxParseBytes int
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		MaxWordPerSection: 250,
		Registry:          DefaultRegistry(),
		MaxParseBytes:     2 << 20,
	}

	for _, fn := range funcs {
		fn(opts)
	}

	return opts
}

func WithMaxWordPerSection(maxWord int) OptionFunc {
	return func(opts *Options) {
		opts.MaxWordPerSection = maxWord
	}
}

func WithRegistry(registry *Registry) OptionFunc {
	return func(opts *Options) {
		opts.Registry = registry
	}
}

func WithMaxParseBytes(maxBytes int) OptionFunc {
	return func(opts *Options) {
		opts.MaxParseBytes = maxBytes
	}
}

// parseTimeoutMicros bounds a single tree-sitter parse (30s), so a
// pathological file degrades to whole-file indexing instead of hanging the
// ingestion task.
const parseTimeoutMicros = 30_000_000

// Parse builds a document whose section tree mirrors the declarations
// (functions, methods, types/classes...) found in a source file.
//
// Parsing is best-effort: when the syntax tree cannot be obtained (timeout,
// unparseable input, oversized file) the document degrades to a single root
// section covering the whole file. Only unknown extensions and invalid
// language definitions are reported as errors.
func Parse(filename string, data []byte, funcs ...OptionFunc) (*Document, error) {
	opts := NewOptions(funcs...)

	ext := strings.ToLower(filepath.Ext(filename))

	lang, exists := opts.Registry.Lookup(ext)
	if !exists {
		return nil, errors.Wrapf(ErrUnsupportedExtension, "no language registered for extension '%s'", ext)
	}

	document := &Document{
		id:          model.NewDocumentID(),
		collections: make([]model.Collection, 0),
		data:        data,
		metadata: map[string]any{
			"type":     "code",
			"language": lang.Name,
		},
	}

	root := &Section{
		document: document,
		level:    0,
		id:       model.NewSectionID(),
		sections: make([]*Section, 0),
		start:    0,
		end:      len(data),
	}

	root.branch = []model.SectionID{root.id}

	document.sections = []*Section{root}

	if ranges, ok := extractDeclarations(lang, data, opts.MaxParseBytes); ok {
		nestSections(document, root, ranges)
	}

	splitOversized(document, root, opts.MaxWordPerSection)

	return document, nil
}

type declRange struct {
	start int
	end   int
}

// extractDeclarations parses the file and returns the byte ranges of the
// declarations captured by the language query, extended over their leading
// doc comments and decorators. The second return value is false when the
// structural extraction was skipped or failed.
func extractDeclarations(lang *Language, data []byte, maxParseBytes int) ([]declRange, bool) {
	if maxParseBytes > 0 && len(data) > maxParseBytes {
		return nil, false
	}

	grammar, query, err := lang.load()
	if err != nil {
		return nil, false
	}

	parser := ts.NewParser(grammar)
	parser.SetTimeoutMicros(parseTimeoutMicros)

	tree, err := parser.Parse(data)
	if err != nil || tree == nil {
		return nil, false
	}

	ranges := make([]declRange, 0)

	for _, match := range query.Execute(tree) {
		for _, capture := range match.Captures {
			node := capture.Node
			start := extendOverLeadingTrivia(node, grammar, data)

			ranges = append(ranges, declRange{
				start: start,
				end:   int(node.EndByte()),
			})
		}
	}

	return ranges, true
}

// leadingTriviaTypes are the node types glued to the following declaration
// (doc comments and decorators/annotations).
var leadingTriviaTypes = map[string]struct{}{
	"comment":       {},
	"line_comment":  {},
	"block_comment": {},
	"decorator":     {},
	"attribute":     {},
}

// extendOverLeadingTrivia walks the previous siblings of a declaration node
// and extends its start over contiguous doc comments and decorators, so that
// a declaration and its documentation land in the same section.
func extendOverLeadingTrivia(node *ts.Node, grammar *ts.Language, data []byte) int {
	start := int(node.StartByte())

	for prev := node.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		if _, isTrivia := leadingTriviaTypes[prev.Type(grammar)]; !isTrivia {
			break
		}

		gap := data[prev.EndByte():start]
		if strings.TrimSpace(string(gap)) != "" || strings.Count(string(gap), "\n") > 1 {
			break
		}

		start = int(prev.StartByte())
	}

	return start
}

// nestSections attaches the declaration ranges to the root section, nesting
// them by byte-range containment (a method range contained in a class range
// becomes a child section of the class section).
func nestSections(document *Document, root *Section, ranges []declRange) {
	slices.SortFunc(ranges, func(a, b declRange) int {
		if a.start != b.start {
			return a.start - b.start
		}

		return b.end - a.end
	})

	ranges = slices.Compact(ranges)

	stack := []*Section{root}

	for _, r := range ranges {
		for len(stack) > 1 && r.start >= stack[len(stack)-1].end {
			stack = stack[:len(stack)-1]
		}

		parent := stack[len(stack)-1]

		if r.start == parent.start && r.end == parent.end {
			continue
		}

		if r.start < parent.start {
			r.start = parent.start
		}

		if r.end > parent.end {
			r.end = parent.end
		}

		section := &Section{
			id:       model.NewSectionID(),
			document: document,
			parent:   parent,
			level:    parent.level + 1,
			sections: make([]*Section, 0),
			start:    r.start,
			end:      r.end,
		}

		section.branch = append(slices.Clone(parent.branch), section.id)

		parent.sections = append(parent.sections, section)

		stack = append(stack, section)
	}
}
