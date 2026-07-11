package markdown

import (
	"github.com/Bornholm/amatl/pkg/markdown/renderer/markdown"
	"github.com/Bornholm/amatl/pkg/markdown/renderer/markdown/node"
	"github.com/yuin/goldmark"
	meta "github.com/yuin/goldmark-meta"
	"github.com/yuin/goldmark/extension"
)

func New() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			meta.Meta,
		),
		goldmark.WithRenderer(markdown.NewRenderer()),
		goldmark.WithRendererOptions(
			markdown.WithNodeRenderers(node.Renderers()),
		),
	)
}
