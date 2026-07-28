package pandoc

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bornholm/amoxtli/blob"
	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/amoxtli/markdown/imagetext"
	"github.com/pkg/errors"
)

// mediaDir is where pandoc extracts the media of a document, relative to the
// working directory of the command. It must stay relative: pandoc writes the
// destinations it rewrites verbatim into the markdown, and a relative path is
// both shorter and safely resolvable under the temporary directory.
const mediaDir = "media"

// Options configures the converter.
type Options struct {
	// InlineMedia extracts the media embedded in the source document and
	// rewrites them as data: URIs in the produced markdown.
	InlineMedia bool
	// MaxImageBytes bounds the size of an inlined image; larger media stay
	// links (which are ignored downstream). 0 defers to the default.
	MaxImageBytes int64
	// Blobs, when set, receives the extracted media, which are then referenced
	// by their internal URI instead of being inlined as data URIs.
	Blobs blob.Store
}

type OptionFunc func(opts *Options)

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		MaxImageBytes: imagetext.DefaultMaxImageBytes,
	}

	for _, fn := range funcs {
		fn(opts)
	}

	return opts
}

// WithInlineMedia extracts the media embedded in the converted documents
// (images of a .docx, an .odt, an .epub...) and inlines them as data: URIs in
// the markdown, making the output self-contained — the temporary extraction
// directory is deleted as soon as the conversion returns.
//
// It is opt-in: inlining inflates the indexed source with base64, which is
// only worth paying for when something downstream reads the images — namely
// the image enrichment (markdown/imagetext.Enrich). maxImageBytes <= 0 keeps
// the default (imagetext.DefaultMaxImageBytes); larger media stay links.
func WithInlineMedia(maxImageBytes int64) OptionFunc {
	return func(opts *Options) {
		opts.InlineMedia = true

		if maxImageBytes > 0 {
			opts.MaxImageBytes = maxImageBytes
		}
	}
}

// WithBlobStore stores the extracted media and references them by their
// internal URI (`amoxtli://images/<hash>`) instead of inlining them as data
// URIs: a much lighter markdown, and the bytes are stored once instead of
// travelling twice. It implies WithInlineMedia.
func WithBlobStore(store blob.Store) OptionFunc {
	return func(opts *Options) {
		opts.InlineMedia = true
		opts.Blobs = store
	}
}

type Converter struct {
	opts *Options
}

// Convert implements convert.Converter.
func (f *Converter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	tempDir, err := os.MkdirTemp(os.TempDir(), "amoxtli-*")
	if err != nil {
		return nil, errors.WithStack(err)
	}

	defer os.RemoveAll(tempDir)

	ext := filepath.Ext(filename)

	source := filepath.Join(tempDir, "file"+ext)
	target := filepath.Join(tempDir, "file.md")

	copy, err := os.Create(source)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if _, err := io.Copy(copy, r); err != nil {
		return nil, errors.WithStack(err)
	}

	args := []string{"--to", "commonmark-raw_html", "--output", target}

	if f.opts.InlineMedia {
		args = append(args, "--extract-media="+mediaDir)
	}

	args = append(args, source)

	cmd := exec.Command("pandoc", args...)

	// Running from the temporary directory keeps the destinations pandoc writes
	// relative to it, so the extracted media resolve — and stay confined —
	// under a directory we own.
	cmd.Dir = tempDir
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout

	if err := cmd.Run(); err != nil {
		return nil, errors.WithStack(err)
	}

	if !f.opts.InlineMedia {
		markdown, err := os.Open(target)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return markdown, nil
	}

	// The media live in the temporary directory this call is about to delete:
	// they must travel inside the markdown itself.
	data, err := os.ReadFile(target)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	inlined, err := imagetext.InlineLocalImagesWithStore(ctx, data, tempDir, f.opts.MaxImageBytes, f.opts.Blobs)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return io.NopCloser(bytes.NewReader(inlined)), nil
}

// SupportedExtensions implements convert.Converter.
func (f *Converter) SupportedExtensions() []string {
	return []string{".docx", ".rtf", ".odt", ".md", ".rst", ".epub", ".html", ".tex", ".txt"}
}

func NewConverter(funcs ...OptionFunc) *Converter {
	return &Converter{opts: NewOptions(funcs...)}
}

var _ convert.Converter = &Converter{}
