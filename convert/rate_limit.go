package convert

import (
	"context"
	"io"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

type RateLimited struct {
	limiter       *rate.Limiter
	fileConverter Converter
}

// Convert implements [Converter].
func (c *RateLimited) Convert(ctx context.Context, filename string, r io.Reader) (io.ReadCloser, error) {
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, errors.WithStack(err)
	}

	return c.fileConverter.Convert(ctx, filename, r)
}

// SupportedExtensions implements [Converter].
func (c *RateLimited) SupportedExtensions() []string {
	return c.fileConverter.SupportedExtensions()
}

func NewRateLimited(fileConverter Converter, interval time.Duration, maxBurst int) *RateLimited {
	return &RateLimited{
		limiter:       rate.NewLimiter(rate.Every(interval), maxBurst),
		fileConverter: fileConverter,
	}
}

var _ Converter = &RateLimited{}
