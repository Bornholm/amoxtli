package markdown

import (
	"bytes"

	"github.com/pkg/errors"
	gmParser "github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

func Trim(markdown []byte) ([]byte, error) {
	md := New()

	transformer := &Transformer{
		transformers: []NodeTransformer{
			StripDataURL,
		},
	}

	md.Parser().AddOptions(gmParser.WithASTTransformers(
		util.Prioritized(
			transformer,
			999,
		),
	))

	reader := text.NewReader(markdown)

	root := md.Parser().Parse(reader)

	var buff bytes.Buffer

	if err := md.Renderer().Render(&buff, markdown, root); err != nil {
		return nil, errors.WithStack(err)
	}

	return buff.Bytes(), nil
}
