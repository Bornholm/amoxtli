package markdown

import (
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

	split := false

	err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			if !split && current.parent != nil && current.start == current.parent.start && current.end == current.parent.end {
				current.parent.sections = slices.DeleteFunc(current.parent.sections, func(s *Section) bool { return s == current })
			}

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
					current.branch = append(current.parent.branch, current.id)
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
					current.branch = append(current.parent.branch, current.id)
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

			current.AppendRange(end)

			currentChunk, err := current.Content()
			if err != nil {
				return ast.WalkStop, errors.WithStack(err)
			}

			if totalWords := len(corpusText.SplitByWords(string(currentChunk))); totalWords < opts.MaxWordPerSection {
				return ast.WalkContinue, nil
			}

			current = &Section{
				document: document,
				id:       model.NewSectionID(),
				level:    current.level + 1,
				sections: make([]*Section, 0),
				parent:   previous,
				start:    current.end,
				end:      end,
			}

			current.branch = append(previous.branch, current.id)

			if split {
				current.level = previous.level
				current.parent = previous.parent
				current.branch = append(previous.parent.branch, current.id)
				previous.parent.sections = append(previous.parent.sections, current)
			} else {
				current.start = previous.end
				current.parent.sections = append(current.parent.sections, current)
			}

			split = true
		}

		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return document, nil
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
