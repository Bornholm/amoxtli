// Package llmx provides reusable decorators around github.com/bornholm/genai
// llm.Client. The RetryClient adds bounded, context-aware retries with
// exponential backoff and optional client-side rate limiting, so a transient
// LLM failure (network blip, provider 429/5xx) no longer fails the whole
// operation that relies on it (HyDE, Judge, grounding evaluation, embeddings).
package llmx

import (
	"context"
	"log/slog"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
	"golang.org/x/time/rate"
)

// Default retry parameters.
const (
	DefaultMaxRetries  = 3
	DefaultBaseBackoff = 500 * time.Millisecond
	DefaultMaxBackoff  = 30 * time.Second
)

// RetryClient decorates an llm.Client with retries (exponential backoff) and an
// optional rate limiter. It is safe for concurrent use as long as the wrapped
// client is.
type RetryClient struct {
	inner       llm.Client
	limiter     *rate.Limiter
	maxRetries  int
	baseBackoff time.Duration
	maxBackoff  time.Duration
	retryable   func(error) bool
}

// Options configures a RetryClient.
type Options struct {
	// MaxRetries is the number of retries attempted after the first failure
	// (so a total of MaxRetries+1 attempts). A negative value disables retries.
	MaxRetries int
	// BaseBackoff is the delay before the first retry; it doubles on each
	// subsequent retry, capped at MaxBackoff.
	BaseBackoff time.Duration
	// MaxBackoff caps the backoff delay.
	MaxBackoff time.Duration
	// Retryable decides whether an error is worth retrying. Defaults to
	// DefaultRetryable (everything except context cancellation/deadline).
	Retryable func(error) bool
	// Limiter, when set, throttles every call (Wait before each attempt,
	// including retries).
	Limiter *rate.Limiter
}

type OptionFunc func(*Options)

// WithMaxRetries sets the number of retries attempted after the first failure.
func WithMaxRetries(n int) OptionFunc {
	return func(o *Options) { o.MaxRetries = n }
}

// WithBackoff sets the base and maximum backoff delays.
func WithBackoff(base, max time.Duration) OptionFunc {
	return func(o *Options) {
		o.BaseBackoff = base
		o.MaxBackoff = max
	}
}

// WithRetryable overrides the predicate deciding whether an error is retryable.
func WithRetryable(fn func(error) bool) OptionFunc {
	return func(o *Options) { o.Retryable = fn }
}

// WithRateLimit throttles calls to at most r events per second with the given
// burst. It applies to every attempt, retries included.
func WithRateLimit(r rate.Limit, burst int) OptionFunc {
	return func(o *Options) { o.Limiter = rate.NewLimiter(r, burst) }
}

// WithLimiter installs a pre-built rate limiter (e.g. shared across clients).
func WithLimiter(limiter *rate.Limiter) OptionFunc {
	return func(o *Options) { o.Limiter = limiter }
}

// DefaultRetryable retries every error except context cancellation and deadline
// expiry, which signal the caller gave up and must not be retried.
func DefaultRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true
}

// NewRetryClient wraps client with retry (and optional rate-limit) behaviour.
func NewRetryClient(client llm.Client, funcs ...OptionFunc) *RetryClient {
	opts := &Options{
		MaxRetries:  DefaultMaxRetries,
		BaseBackoff: DefaultBaseBackoff,
		MaxBackoff:  DefaultMaxBackoff,
		Retryable:   DefaultRetryable,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	if opts.Retryable == nil {
		opts.Retryable = DefaultRetryable
	}

	return &RetryClient{
		inner:       client,
		limiter:     opts.Limiter,
		maxRetries:  opts.MaxRetries,
		baseBackoff: opts.BaseBackoff,
		maxBackoff:  opts.MaxBackoff,
		retryable:   opts.Retryable,
	}
}

// ChatCompletion implements llm.ChatCompletionClient with retries.
func (c *RetryClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	var res llm.ChatCompletionResponse
	err := c.do(ctx, "ChatCompletion", func() error {
		var e error
		res, e = c.inner.ChatCompletion(ctx, funcs...)
		return e
	})
	return res, err
}

// ChatCompletionStream implements llm.ChatCompletionStreamingClient. Only the
// call opening the stream is retried; once the channel is returned, chunks flow
// through unchanged (a mid-stream failure cannot be safely retried).
func (c *RetryClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	var ch <-chan llm.StreamChunk
	err := c.do(ctx, "ChatCompletionStream", func() error {
		var e error
		ch, e = c.inner.ChatCompletionStream(ctx, funcs...)
		return e
	})
	return ch, err
}

// Embeddings implements llm.EmbeddingsClient with retries.
func (c *RetryClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	var res llm.EmbeddingsResponse
	err := c.do(ctx, "Embeddings", func() error {
		var e error
		res, e = c.inner.Embeddings(ctx, inputs, funcs...)
		return e
	})
	return res, err
}

// do runs fn, retrying on retryable errors with exponential backoff, honouring
// the rate limiter and the context on every attempt and wait.
func (c *RetryClient) do(ctx context.Context, op string, fn func() error) error {
	backoff := c.baseBackoff

	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return errors.WithStack(err)
		}

		if c.limiter != nil {
			if err := c.limiter.Wait(ctx); err != nil {
				return errors.WithStack(err)
			}
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}

		if attempt >= c.maxRetries || !c.retryable(lastErr) {
			return errors.WithStack(lastErr)
		}

		slog.WarnContext(ctx, "llmx: LLM call failed, retrying",
			slog.String("op", op),
			slog.Int("attempt", attempt+1),
			slog.Duration("backoff", backoff),
			slog.Any("error", lastErr),
		)

		select {
		case <-ctx.Done():
			return errors.WithStack(ctx.Err())
		case <-time.After(backoff):
		}

		if backoff = backoff * 2; backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}

var _ llm.Client = &RetryClient{}
