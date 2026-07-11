package genai

import (
	"context"
	"io"

	"github.com/bornholm/amoxtli/convert"
	"github.com/bornholm/genai/extract"
	"github.com/pkg/errors"
)

type Converter struct {
	extract    extract.TextClient
	extensions []string
}

// Convert implements convert.Converter.
func (f *Converter) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	res, err := f.extract.Text(ctx, extract.WithReader(r), extract.WithFilename(filename))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return io.NopCloser(res.Output()), nil
}

// SupportedExtensions implements convert.Converter.
func (f *Converter) SupportedExtensions() []string {
	return f.extensions
}

func NewConverter(extract extract.TextClient, extensions ...string) *Converter {
	return &Converter{extract: extract, extensions: extensions}
}

var _ convert.Converter = &Converter{}
