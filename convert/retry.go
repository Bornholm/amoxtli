package convert

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/bornholm/genai/extract"
	"github.com/pkg/errors"
)

type Retry struct {
	baseDelay     time.Duration
	maxRetries    int
	fileConverter Converter
}

// Convert implements [Converter].
func (c *Retry) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	backoff := c.baseDelay
	maxRetries := c.maxRetries
	retries := 0

	for {
		reader, err := c.fileConverter.Convert(ctx, filename, r)
		if err != nil {
			// A converter is expected to return a nil reader on failure, but
			// closing a non-nil one keeps a partially successful attempt from
			// leaking its handle across retries.
			if reader != nil {
				if cerr := reader.Close(); cerr != nil {
					slog.DebugContext(ctx, "could not close failed conversion reader", slog.Any("error", errors.WithStack(cerr)))
				}
			}

			if retries >= maxRetries {
				return nil, errors.WithStack(err)
			}

			if errors.Is(err, extract.ErrRateLimit) {
				// The failed attempt consumed all or part of r. Retrying on a
				// drained reader would silently convert an empty input, so a
				// retry is only possible when the source can be rewound.
				seeker, ok := r.(io.Seeker)
				if !ok {
					return nil, errors.WithStack(err)
				}

				slog.DebugContext(ctx, "request failed, will retry", slog.Int("retries", retries), slog.Duration("backoff", backoff), slog.Any("error", errors.WithStack(err)))

				retries++
				time.Sleep(backoff)
				backoff *= 2

				if _, serr := seeker.Seek(0, io.SeekStart); serr != nil {
					return nil, errors.WithStack(serr)
				}

				continue
			}

			return nil, errors.WithStack(err)
		}

		return reader, nil
	}
}

// SupportedExtensions implements [Converter].
func (c *Retry) SupportedExtensions() []string {
	return c.fileConverter.SupportedExtensions()
}

func NewRetry(fileConverter Converter, baseDelay time.Duration, maxRetries int) *Retry {
	return &Retry{
		baseDelay:     baseDelay,
		maxRetries:    maxRetries,
		fileConverter: fileConverter,
	}
}

var _ Converter = &Retry{}
