package convert

import (
	"context"
	"errors"
	"io"
)

var (
	ErrNotSupported = errors.New("not supported")
)

// Converter converts a given file to its markdown equivalent.
type Converter interface {
	SupportedExtensions() []string
	Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error)
}
