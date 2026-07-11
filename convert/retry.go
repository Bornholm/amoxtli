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
			if retries >= maxRetries {
				return nil, errors.WithStack(err)
			}

			if errors.Is(err, extract.ErrRateLimit) {
				slog.DebugContext(ctx, "request failed, will retry", slog.Int("retries", retries), slog.Duration("backoff", backoff), slog.Any("error", errors.WithStack(err)))

				retries++
				time.Sleep(backoff)
				backoff *= 2
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
