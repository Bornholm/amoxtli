package markdown

import (
	"log/slog"
	"net/url"
	"slices"

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

	split := false

	// splitAfter opens a new section once s holds MaxWordPerSection words and
	// returns it, or returns s while it has room left. The new section starts
	// where s ends, so the node that filled s stays in s.
	splitAfter := func(s *Section, end int) (*Section, error) {
		chunk, err := s.Content()
		if err != nil {
			return nil, errors.WithStack(err)
		}

		if totalWords := len(corpusText.SplitByWords(string(chunk))); totalWords < opts.MaxWordPerSection {
			return s, nil
		}

		next := &Section{
			document: document,
			id:       model.NewSectionID(),
			level:    s.level + 1,
			sections: make([]*Section, 0),
			parent:   s,
			start:    s.end,
			end:      end,
		}

		next.branch = append(slices.Clone(s.branch), next.id)

		if split {
			next.level = s.level
			next.parent = s.parent
			next.branch = append(slices.Clone(s.parent.branch), next.id)
			s.parent.sections = append(s.parent.sections, next)
		} else {
			s.sections = append(s.sections, next)
		}

		split = true

		return next, nil
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

				// A heading longer than MaxWordPerSection is split at the end
				// of its line, so the next block opens a section of its own.
				next, err := splitAfter(current, lastLine.Stop)
				if err != nil {
					return ast.WalkStop, errors.WithStack(err)
				}

				current = next
			} else {
				return ast.WalkContinue, nil
			}

		default:
			var (
				end int
			)

			if n.Type() == ast.TypeBlock {
				if lines := n.Lines(); lines.Len() > 0 {
					lastLine := lines.At(lines.Len() - 1)
					end = lastLine.Stop
					if _, isCodeBlock := n.(*ast.FencedCodeBlock); isCodeBlock {
						end += 4
					}
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

			// A node inside the current section adds nothing to it. The
			// inlines of a heading are such nodes, since the heading branch
			// already covers the whole line and counts its words. Splitting on
			// one would open a section that starts past its own end.
			if end <= current.end {
				return ast.WalkContinue, nil
			}

			current.AppendRange(end)

			next, err := splitAfter(current, end)
			if err != nil {
				return ast.WalkStop, errors.WithStack(err)
			}

			current = next
		}

		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	document.sections = pruneRedundant(document.sections, nil)

	return document, nil
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
