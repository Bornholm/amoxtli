package markdown

import (
	"strings"

	"github.com/pkg/errors"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
)

type NodeTransformer interface {
	Transform(n ast.Node) error
}

type NodeTransformerFunc func(n ast.Node) error

func (f NodeTransformerFunc) Transform(n ast.Node) error {
	if err := f(n); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

type Transformer struct {
	transformers []NodeTransformer
	err          error
}

func (t *Transformer) Transform(root *ast.Document, reader text.Reader, pc parser.Context) {
	err := ast.Walk(root, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}

		for _, nodeTransformer := range t.transformers {
			if err := nodeTransformer.Transform(n); err != nil {
				return ast.WalkStop, errors.WithStack(err)
			}
		}

		return ast.WalkContinue, nil
	})
	if err != nil {
		t.err = errors.WithStack(err)
	}
}

func (t *Transformer) Error() error {
	return errors.WithStack(t.err)
}

var StripDataURL NodeTransformerFunc = func(n ast.Node) error {
	stripDataURL := func(destination []byte) []byte {
		if strings.HasPrefix(string(destination), "data:") {
			destination = []byte("#stripped")
		}
		return destination
	}

	switch typ := n.(type) {
	case *ast.Image:
		typ.Destination = stripDataURL(typ.Destination)
	case *ast.Link:
		typ.Destination = stripDataURL(typ.Destination)
	}

	return nil
}
