// Package llmx provides reusable decorators around github.com/bornholm/genai
// llm.Client. The RetryClient adds bounded, context-aware retries with
// exponential backoff and optional client-side rate limiting, so a transient
// LLM failure (network blip, provider 429/5xx) no longer fails the whole
// operation that relies on it (HyDE, Judge, grounding evaluation, embeddings).
package llmx

import (
	"context"
	"log/slog"
	"math/rand/v2"
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
	retryAfter  func(error) (time.Duration, bool)
}

// Options configures a RetryClient.
type Options struct {
	// MaxRetries is the number of retries attempted after the first failure
	// (so a total of MaxRetries+1 attempts). A negative value disables retries.
	MaxRetries int
	// BaseBackoff is the delay before the first retry; it doubles on each
	// subsequent retry, capped at MaxBackoff. The delay actually waited is
	// drawn from [backoff/2, backoff) — see jitter.
	BaseBackoff time.Duration
	// MaxBackoff caps the backoff delay.
	MaxBackoff time.Duration
	// Retryable decides whether an error is worth retrying. Defaults to
	// DefaultRetryable (everything except context cancellation/deadline).
	Retryable func(error) bool
	// RetryAfter extracts the delay a provider explicitly asked us to wait
	// (an HTTP Retry-After header on a 429/503). When it reports a delay, that
	// delay wins over the computed backoff — the provider knows when its quota
	// window reopens, we only guess. Defaults to DefaultRetryAfter.
	RetryAfter func(error) (time.Duration, bool)
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

// WithRetryAfter overrides how a provider-mandated retry delay is extracted from
// an error (see Options.RetryAfter).
func WithRetryAfter(fn func(error) (time.Duration, bool)) OptionFunc {
	return func(o *Options) { o.RetryAfter = fn }
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

// RetryAfterError is implemented by errors carrying a provider-mandated retry
// delay, typically parsed from an HTTP Retry-After header. Providers whose
// errors implement it are honoured automatically by DefaultRetryAfter.
type RetryAfterError interface {
	error
	// RetryAfter returns the delay to wait before retrying.
	RetryAfter() time.Duration
}

// DefaultRetryAfter reports the delay carried by the first RetryAfterError in
// err's chain, if any. A non-positive delay is ignored: it carries no
// information, and honouring it would turn the backoff into a busy loop.
func DefaultRetryAfter(err error) (time.Duration, bool) {
	var retryAfter RetryAfterError
	if !errors.As(err, &retryAfter) {
		return 0, false
	}

	d := retryAfter.RetryAfter()
	if d <= 0 {
		return 0, false
	}

	return d, true
}

// NewRetryClient wraps client with retry (and optional rate-limit) behaviour.
func NewRetryClient(client llm.Client, funcs ...OptionFunc) *RetryClient {
	opts := &Options{
		MaxRetries:  DefaultMaxRetries,
		BaseBackoff: DefaultBaseBackoff,
		MaxBackoff:  DefaultMaxBackoff,
		Retryable:   DefaultRetryable,
		RetryAfter:  DefaultRetryAfter,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	if opts.Retryable == nil {
		opts.Retryable = DefaultRetryable
	}
	if opts.RetryAfter == nil {
		opts.RetryAfter = DefaultRetryAfter
	}

	return &RetryClient{
		inner:       client,
		limiter:     opts.Limiter,
		maxRetries:  opts.MaxRetries,
		baseBackoff: opts.BaseBackoff,
		maxBackoff:  opts.MaxBackoff,
		retryable:   opts.Retryable,
		retryAfter:  opts.RetryAfter,
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

		// A provider-mandated delay is authoritative; otherwise the exponential
		// backoff is jittered so that several clients (or several concurrent
		// calls of this one) that failed together do not retry in lockstep and
		// re-create the very burst that rate-limited them.
		wait, mandated := c.retryAfter(lastErr)
		if mandated {
			wait = min(wait, c.maxBackoff)
		} else {
			wait = jitter(backoff)
		}

		slog.WarnContext(ctx, "llmx: LLM call failed, retrying",
			slog.String("op", op),
			slog.Int("attempt", attempt+1),
			slog.Duration("backoff", wait),
			slog.Bool("retryAfter", mandated),
			slog.Any("error", lastErr),
		)

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.WithStack(ctx.Err())
		case <-timer.C:
		}

		if backoff = backoff * 2; backoff > c.maxBackoff {
			backoff = c.maxBackoff
		}
	}
}

// jitter spreads a backoff delay over [d/2, d) — "equal jitter". Half the delay
// is kept deterministic so the backoff still grows monotonically in expectation,
// while the random half decorrelates retries across callers.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}

	half := d / 2

	return half + time.Duration(rand.Int64N(int64(half)+1))
}

var _ llm.Client = &RetryClient{}
