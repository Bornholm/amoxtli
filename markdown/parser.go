package markdown

import (
	"log/slog"
	"net/url"
	"slices"

	"github.com/bornholm/amoxtli/internal/cdc"
	corpusText "github.com/bornholm/amoxtli/internal/text"
	"github.com/bornholm/amoxtli/model"
	"github.com/pkg/errors"
	meta "github.com/yuin/goldmark-meta"
	"github.com/yuin/goldmark/ast"
	gmParser "github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type Options struct {
	MaxWordPerSection int
	Transformers      []NodeTransformer
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		MaxWordPerSection: 250,
		Transformers:      make([]NodeTransformer, 0),
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

func WithNodeTransformers(transformers ...NodeTransformer) OptionFunc {
	return func(opts *Options) {
		opts.Transformers = transformers
	}
}

func Parse(data []byte, funcs ...OptionFunc) (*Document, error) {
	opts := NewOptions(funcs...)

	md := New()

	context := gmParser.NewContext()
	parser := md.Parser()

	transformer := &Transformer{
		transformers: opts.Transformers,
	}

	parser.AddOptions(gmParser.WithASTTransformers(
		util.Prioritized(
			transformer,
			999,
		),
	))

	root := parser.Parse(
		text.NewReader(data),
		gmParser.WithContext(context),
	)

	if err := transformer.Error(); err != nil {
		return nil, errors.WithStack(err)
	}

	document := &Document{
		id:          model.NewDocumentID(),
		collections: make([]model.Collection, 0),
		data:        data,
	}

	current := &Section{
		document: document,
		level:    0,
		id:       model.NewSectionID(),
		sections: make([]*Section, 0),
	}

	current.branch = []model.SectionID{current.id}

	document.sections = []*Section{current}

	metadata := meta.Get(context)
	if rawSource, exists := metadata["source"]; exists {
		source, err := url.Parse(rawSource.(string))
		if err != nil {
			return nil, errors.Wrapf(err, "could not parse metadata source url '%v'", rawSource)
		}

		document.source = source
	}

	if documentMetadata := hoistMetadata(metadata); len(documentMetadata) > 0 {
		document.SetMetadata(documentMetadata)
	}

	policy := cdc.NewPolicy(opts.MaxWordPerSection)

	// split tells whether the current section was itself opened by a split:
	// the first split of a heading section nests below it, the next ones are
	// its siblings.
	split := false

	// previousBlock is the last block appended to the current section: a cut
	// never follows a heading-like one (see cdc.Anchored).
	previousBlock := ""

	// currentWords caches the word count of the current section, -1 when its
	// range changed since it was counted: counting renders the section.
	currentWords := -1

	countWords := func() (int, error) {
		if currentWords < 0 {
			words, err := sectionWords(current)
			if err != nil {
				return 0, errors.WithStack(err)
			}
			currentWords = words
		}
		return currentWords, nil
	}

	// splitSection closes previous and opens the section that follows it.
	splitSection := func(previous *Section) *Section {
		next := &Section{
			document: document,
			id:       model.NewSectionID(),
			level:    previous.level + 1,
			sections: make([]*Section, 0),
			parent:   previous,
			start:    previous.end,
			end:      previous.end,
		}

		// Cloned: siblings must not share, and overwrite, a backing array.
		next.branch = append(slices.Clone(previous.branch), next.id)

		if split {
			next.level = previous.level
			next.parent = previous.parent
			next.branch = append(slices.Clone(previous.parent.branch), next.id)
			previous.parent.sections = append(previous.parent.sections, next)
		} else {
			previous.sections = append(previous.sections, next)
		}

		split = true
		currentWords = -1

		return next
	}

	err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		previous := current

		switch el := n.(type) {
		case *ast.Document:
			// No op
		case *ast.Heading:
			split = false
			previousBlock = ""
			currentWords = -1

			if uint(el.Level) > current.level {
				current = &Section{
					id:       model.NewSectionID(),
					document: document,
					level:    uint(el.Level),
					sections: make([]*Section, 0),
				}

				current.parent = findClosestAncestor(previous, uint(el.Level))

				if current.parent != nil {
					current.parent.sections = append(current.parent.sections, current)
					current.branch = append(slices.Clone(current.parent.branch), current.id)
				} else {
					document.sections = append(document.sections, current)
					current.branch = []model.SectionID{current.id}
				}
			} else {
				current = &Section{
					document: document,
					id:       model.NewSectionID(),
					level:    uint(el.Level),
					sections: make([]*Section, 0),
				}

				current.parent = findClosestAncestor(previous, uint(el.Level))

				if current.parent != nil {
					current.parent.sections = append(current.parent.sections, current)
					current.branch = append(slices.Clone(current.parent.branch), current.id)
				} else {
					document.sections = append(document.sections, current)
					current.branch = []model.SectionID{current.id}
				}
			}

			if lines := n.Lines(); lines.Len() > 0 {
				firstLine := lines.At(0)
				lastLine := lines.At(lines.Len() - 1)
				current.start = firstLine.Start - (el.Level + 1)
				current.AppendRange(lastLine.Stop)

				// A heading longer than the bound is split at the end of its
				// line, so the next block opens a section of its own.
				currentWords = -1
				words, err := countWords()
				if err != nil {
					return ast.WalkStop, errors.WithStack(err)
				}
				if words >= policy.Max {
					current = splitSection(current)
				}
			} else {
				return ast.WalkContinue, nil
			}

		default:
			var (
				start   int
				end     int
				isBlock bool
			)

			if n.Type() == ast.TypeBlock {
				if lines := n.Lines(); lines.Len() > 0 {
					start = lines.At(0).Start
					lastLine := lines.At(lines.Len() - 1)
					end = lastLine.Stop
					if _, isCodeBlock := n.(*ast.FencedCodeBlock); isCodeBlock {
						end += 4
					}
					isBlock = true
				} else {
					return ast.WalkContinue, nil
				}
			} else if n.Type() == ast.TypeInline {
				if text, ok := n.(*ast.Text); ok {
					end = text.Segment.Stop
				}
			} else {
				return ast.WalkContinue, nil
			}

			// Content-defined cut (see cdc.Policy.CutBefore): an anchor block
			// opens a new section once the current one holds half the bound.
			// Packing blocks up to the bound alone makes every cut depend on all
			// the blocks before it, so one article inserted at the top of a long
			// section would shift — and re-embed — every section after it. Only
			// the blocks not covered yet count: the paragraphs of a list item
			// were appended along with their list.
			uncovered := isBlock && start >= current.end
			block := ""
			if uncovered {
				block = string(data[start:min(end, len(data))])
			}

			// Counting the section words is costly: only for a block that may
			// open a new section.
			if uncovered && (cdc.IsLead(block) || cdc.Anchored(previousBlock, block)) {
				words, err := countWords()
				if err != nil {
					return ast.WalkStop, errors.WithStack(err)
				}

				if policy.CutBefore(words, len(corpusText.SplitByWords(block)), previousBlock, block) {
					current = splitSection(current)
				}
			}

			if uncovered {
				previousBlock = block
			}

			// A node inside the current section adds nothing to it. The
			// inlines of a heading are such nodes, since the heading branch
			// already covers the whole line and counts its words. Splitting on
			// one would open a section that starts past its own end.
			if end <= current.end {
				return ast.WalkContinue, nil
			}

			current.AppendRange(end)
			currentWords = -1

			words, err := countWords()
			if err != nil {
				return ast.WalkStop, errors.WithStack(err)
			}

			if words < policy.Max {
				return ast.WalkContinue, nil
			}

			current = splitSection(current)
		}

		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	document.sections = pruneRedundant(document.sections, nil)

	return document, nil
}

// sectionWords counts the words of a section's content.
func sectionWords(s *Section) (int, error) {
	content, err := s.Content()
	if err != nil {
		return 0, errors.WithStack(err)
	}

	return len(corpusText.SplitByWords(string(content))), nil
}

// pruneRedundant drops the sections covering exactly the same range as their
// parent — the section of a heading opening a document duplicates the whole
// document — and puts their children in their place rather than dropping the
// branch with them.
//
// It also drops the sections whose range is empty. A word-count split opens a
// section right after the node that filled the previous one, and nothing fills
// it when that node is the last one, or when a heading of the same or a higher
// level comes next. Such a section holds no text, yet the indexes would store it.
// The root section stays, even empty: a document always has one. An inverted
// range is a parser bug, so it stays too, where the tests can see it.
//
// It runs on the finished tree instead of during the walk: a heading looks
// redundant the moment it is entered, before the sub-sections that make it a
// branch even exist, so pruning it there took its whole subtree along. A
// document starting with a title — most of them — was left with a single flat
// section holding the entire file, indexed as one chunk and impossible to
// return in a readable excerpt afterwards.
func pruneRedundant(sections []*Section, parent *Section) []*Section {
	kept := make([]*Section, 0, len(sections))

	for _, s := range sections {
		s.sections = pruneRedundant(s.sections, s)

		if parent != nil && (s.start == s.end || (s.start == parent.start && s.end == parent.end)) {
			for _, child := range s.sections {
				child.parent = parent
				rebranch(child, parent.branch)
			}

			kept = append(kept, s.sections...)

			continue
		}

		kept = append(kept, s)
	}

	return kept
}

// rebranch rewrites the ancestry a lifted section carries, so that a branch
// keeps naming the sections one actually has to walk through to reach it.
func rebranch(s *Section, parentBranch []model.SectionID) {
	s.branch = append(slices.Clone(parentBranch), s.id)
	for _, child := range s.sections {
		rebranch(child, s.branch)
	}
}

// hoistMetadata lifts the YAML frontmatter into the document metadata, so that
// a converter which only produces markdown (e.g. convert/vision emitting
// `type: image`) can tag its documents for search-time filtering — symmetrical
// to what sourcecode.Parse does with type=code/language.
//
// Only scalars survive: metadata are compared as JSON values by the index
// backends, so values are normalized to string/float64/bool and composite
// values (lists, maps) are dropped. The "source" key is excluded: it is the
// document source URL, handled separately.
func hoistMetadata(frontmatter map[string]any) map[string]any {
	if len(frontmatter) == 0 {
		return nil
	}

	metadata := make(map[string]any, len(frontmatter))

	for key, rawValue := range frontmatter {
		if key == "source" {
			continue
		}

		value, ok := normalizeMetadataValue(rawValue)
		if !ok {
			slog.Debug("ignoring non-scalar frontmatter metadata", slog.String("key", key))
			continue
		}

		metadata[key] = value
	}

	return metadata
}

// normalizeMetadataValue maps a YAML scalar onto its JSON equivalent,
// reporting false for composite values.
func normalizeMetadataValue(value any) (any, bool) {
	switch v := value.(type) {
	case string:
		return v, true
	case bool:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case float32:
		return float64(v), true
	case float64:
		return v, true
	default:
		return nil, false
	}
}

func findClosestAncestor(from *Section, level uint) *Section {
	if from == nil {
		return nil
	}

	if from.level < level {
		return from
	}

	return findClosestAncestor(from.parent, level)
}
