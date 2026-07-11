package convert

import (
	"context"
	"io"
	"path/filepath"
	"slices"

	"github.com/pkg/errors"
)

type Routed struct {
	supportedExtensions []string
	converters          []Converter
}

// Convert implements Converter.
func (c *Routed) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	ext := filepath.Ext(filename)
	for _, c := range c.converters {
		if !slices.Contains(c.SupportedExtensions(), ext) {
			continue
		}

		readCloser, err := c.Convert(ctx, filename, r)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return readCloser, nil
	}

	return nil, errors.WithStack(ErrNotSupported)
}

// SupportedExtensions implements Converter.
func (c *Routed) SupportedExtensions() []string {
	return c.supportedExtensions
}

func NewRouted(converters ...Converter) *Routed {
	supportedExtensions := make([]string, 0)
	for _, c := range converters {
		supportedExtensions = append(supportedExtensions, c.SupportedExtensions()...)
	}

	return &Routed{
		supportedExtensions: supportedExtensions,
		converters:          converters,
	}
}

var _ Converter = &Routed{}
