// Package vision converts standalone image files (.png, .jpg, ...) to
// markdown by describing them with a vision LLM. The emitted markdown carries
// a `type: image` frontmatter and is then parsed, chunked and indexed like any
// other document — no change to the indexing layer.
package vision

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/vision"
	"github.com/pkg/errors"
)

// DefaultExtensions are the image formats accepted by the mainstream vision
// providers, and the extensions routed to this converter when none are given.
var DefaultExtensions = []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}

// Converter turns an image file into its markdown description.
type Converter struct {
	describer  vision.Describer
	extensions []string
	blobs      blob.Store
}

// Option configures a Converter.
type Option func(*Converter)

// WithBlobStore stores the converted image and references it in the emitted
// markdown as `![title](amoxtli://images/<hash>)`, so an agent can ask for the
// image back (see the fetch_image MCP tool) instead of only reading about it.
// It also fixes the dead-link problem: the image is served from the store even
// if the original file has moved.
func WithBlobStore(store blob.Store) Option {
	return func(c *Converter) {
		c.blobs = store
	}
}

// NewConverter builds a converter routing extensions (DefaultExtensions when
// none are given) to describer.
func NewConverter(describer vision.Describer, extensions ...string) *Converter {
	return NewConverterWithOptions(describer, extensions, nil)
}

// NewConverterWithOptions is NewConverter with the extra options; it exists
// because the extension list is variadic.
func NewConverterWithOptions(describer vision.Describer, extensions []string, funcs []Option) *Converter {
	if len(extensions) == 0 {
		extensions = DefaultExtensions
	}

	normalized := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		normalized = append(normalized, strings.ToLower(ext))
	}

	converter := &Converter{
		describer:  describer,
		extensions: normalized,
	}

	for _, fn := range funcs {
		fn(converter)
	}

	return converter
}

// SupportedExtensions implements convert.Converter.
func (c *Converter) SupportedExtensions() []string {
	return c.extensions
}

// Convert implements convert.Converter.
func (c *Converter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if !containsFold(c.extensions, ext) {
		return nil, errors.WithStack(convert.ErrNotSupported)
	}

	maxBytes := c.maxSourceBytes()

	// Read one byte past the limit: a file too large to even be shrunk is
	// rejected here, before any (billable) call to the model. Between the model
	// limit and this one the describer re-encodes the image itself, so the
	// bytes are read in full.
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if int64(len(data)) > maxBytes {
		return nil, errors.Wrapf(vision.ErrImageTooLarge, "image '%s' exceeds the %d bytes limit", filename, maxBytes)
	}

	mediaType := mimeType(ext, data)

	description, err := c.describer.Describe(ctx, mediaType, data)
	if err != nil {
		return nil, errors.Wrapf(err, "could not describe image '%s'", filename)
	}

	// Storing is best-effort: an unavailable blob store must not cost the
	// description we just paid for. The document is simply emitted without an
	// internal reference, exactly as when no store is configured.
	var uri string
	if c.blobs != nil {
		hash, err := c.blobs.Put(ctx, mediaType, data)
		if err != nil {
			slog.WarnContext(ctx, "could not store image, indexing its description alone",
				slog.String("filename", filename),
				slog.Any("error", errors.WithStack(err)),
			)
		} else {
			uri = blob.URI(hash)
		}
	}

	return io.NopCloser(bytes.NewReader(render(filename, description, uri))), nil
}

// maxImageBytes reports the limit of the describer when it exposes one.
func (c *Converter) maxImageBytes() int64 {
	if describer, ok := c.describer.(interface{ MaxImageBytes() int64 }); ok {
		if maxBytes := describer.MaxImageBytes(); maxBytes > 0 {
			return maxBytes
		}
	}

	return vision.DefaultMaxImageBytes
}

// maxSourceBytes reports the largest image the describer accepts before
// shrinking it; beyond that the file is not even read.
func (c *Converter) maxSourceBytes() int64 {
	if describer, ok := c.describer.(interface{ MaxSourceBytes() int64 }); ok {
		if maxBytes := describer.MaxSourceBytes(); maxBytes > 0 {
			return maxBytes
		}
	}

	return max(vision.DefaultMaxSourceBytes, c.maxImageBytes())
}

// render lays out the description as markdown. The frontmatter `type: image`
// is hoisted into the document metadata by markdown.Parse, making the document
// filterable with `--filter type=image`.
func render(filename string, description *vision.Description, uri string) []byte {
	title := description.Title
	if title == "" {
		base := filepath.Base(filename)
		title = strings.TrimSuffix(base, filepath.Ext(base))
	}

	var buf bytes.Buffer

	buf.WriteString("---\ntype: image\n---\n\n# ")
	buf.WriteString(title)
	buf.WriteString("\n")

	// The internal URI is not a data URI, so it survives StripDataURL and
	// reaches the agent inside the rendered chunk.
	if uri != "" {
		buf.WriteString("\n![")
		buf.WriteString(title)
		buf.WriteString("](")
		buf.WriteString(uri)
		buf.WriteString(")\n")
	}

	if description.Description != "" {
		buf.WriteString("\n")
		buf.WriteString(description.Description)
		buf.WriteString("\n")
	}

	if description.Text != "" {
		buf.WriteString("\n## Texte visible\n\n")
		buf.WriteString(description.Text)
		buf.WriteString("\n")
	}

	return buf.Bytes()
}

// mimeType resolves the media type from the extension, falling back to content
// sniffing when the extension is unknown to the system mime database.
func mimeType(ext string, data []byte) string {
	if t := mime.TypeByExtension(ext); t != "" {
		if media, _, err := mime.ParseMediaType(t); err == nil {
			return media
		}
	}

	detected := http.DetectContentType(data)
	if media, _, err := mime.ParseMediaType(detected); err == nil {
		return media
	}

	return detected
}

func containsFold(values []string, search string) bool {
	for _, v := range values {
		if strings.EqualFold(v, search) {
			return true
		}
	}

	return false
}

var _ convert.Converter = &Converter{}
