package llmx

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

func TestRetryClientRetriesThenSucceeds(t *testing.T) {
	m := &mockClient{failUntil: 2, err: errors.New("transient")}
	c := NewRetryClient(m, WithMaxRetries(3), WithBackoff(time.Millisecond, 5*time.Millisecond))

	if _, err := c.Embeddings(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	if got := m.calls(); got != 3 {
		t.Errorf("calls = %d, want 3 (2 failures + 1 success)", got)
	}
}

func TestRetryClientExhaustsRetries(t *testing.T) {
	m := &mockClient{failUntil: 100, err: errors.New("down")}
	c := NewRetryClient(m, WithMaxRetries(2), WithBackoff(time.Millisecond, 5*time.Millisecond))

	if _, err := c.Embeddings(context.Background(), []string{"x"}); err == nil {
		t.Fatal("expected error after exhausting retries")
	}

	if got := m.calls(); got != 3 {
		t.Errorf("calls = %d, want 3 (1 initial + 2 retries)", got)
	}
}

func TestRetryClientDoesNotRetryNonRetryable(t *testing.T) {
	m := &mockClient{failUntil: 100, err: context.Canceled}
	c := NewRetryClient(m, WithMaxRetries(5), WithBackoff(time.Millisecond, 5*time.Millisecond))

	if _, err := c.ChatCompletion(context.Background()); err == nil {
		t.Fatal("expected error")
	}

	if got := m.calls(); got != 1 {
		t.Errorf("calls = %d, want 1 (context.Canceled must not be retried)", got)
	}
}

func TestRetryClientHonorsContextDuringBackoff(t *testing.T) {
	m := &mockClient{failUntil: 1000, err: errors.New("down")}
	c := NewRetryClient(m, WithMaxRetries(1000), WithBackoff(50*time.Millisecond, time.Second))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, err := c.Embeddings(ctx, []string{"x"}); err == nil {
		t.Fatal("expected error on context timeout")
	}

	// With a 50ms base backoff and a 30ms deadline, only a couple of attempts
	// can happen before the context aborts the wait.
	if got := m.calls(); got > 3 {
		t.Errorf("calls = %d, expected the context deadline to abort retries early", got)
	}
}

func TestRetryClientCustomRetryable(t *testing.T) {
	sentinel := errors.New("do-not-retry")
	m := &mockClient{failUntil: 100, err: sentinel}
	c := NewRetryClient(m,
		WithMaxRetries(5),
		WithBackoff(time.Millisecond, 5*time.Millisecond),
		WithRetryable(func(err error) bool { return !errors.Is(err, sentinel) }),
	)

	if _, err := c.ChatCompletion(context.Background()); err == nil {
		t.Fatal("expected error")
	}

	if got := m.calls(); got != 1 {
		t.Errorf("calls = %d, want 1 (custom predicate marks the error non-retryable)", got)
	}
}

func TestJitterStaysWithinBounds(t *testing.T) {
	const d = 100 * time.Millisecond

	for range 1000 {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%s) = %s, want within [%s, %s]", d, got, d/2, d)
		}
	}

	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %s, want 0", got)
	}
}

func TestJitterSpreadsRetries(t *testing.T) {
	const d = time.Second

	seen := map[time.Duration]struct{}{}
	for range 100 {
		seen[jitter(d)] = struct{}{}
	}

	// The whole point of jittering is that concurrent clients do not wake up
	// together: a constant delay would collapse to a single distinct value.
	if len(seen) < 50 {
		t.Errorf("jitter produced %d distinct delays out of 100 draws, expected them to be spread", len(seen))
	}
}

// retryAfterError carries a provider-mandated delay, like a parsed Retry-After
// header would.
type retryAfterError struct {
	delay time.Duration
}

func (e *retryAfterError) Error() string             { return "rate limited" }
func (e *retryAfterError) RetryAfter() time.Duration { return e.delay }

func TestRetryClientHonorsRetryAfter(t *testing.T) {
	m := &mockClient{failUntil: 1, err: &retryAfterError{delay: 40 * time.Millisecond}}
	// A base backoff far below the mandated delay: if Retry-After were ignored,
	// the retry would happen almost immediately.
	c := NewRetryClient(m, WithMaxRetries(2), WithBackoff(time.Millisecond, time.Minute))

	start := time.Now()
	if _, err := c.Embeddings(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}
	elapsed := time.Since(start)

	if elapsed < 40*time.Millisecond {
		t.Errorf("retried after %s, want at least the mandated 40ms", elapsed)
	}
	if got := m.calls(); got != 2 {
		t.Errorf("calls = %d, want 2", got)
	}
}

func TestRetryClientCapsRetryAfterAtMaxBackoff(t *testing.T) {
	m := &mockClient{failUntil: 1, err: &retryAfterError{delay: time.Hour}}
	c := NewRetryClient(m, WithMaxRetries(2), WithBackoff(time.Millisecond, 10*time.Millisecond))

	start := time.Now()
	if _, err := c.Embeddings(context.Background(), []string{"x"}); err != nil {
		t.Fatalf("unexpected error: %+v", err)
	}

	// An absurd (or hostile) Retry-After must not pin the caller for an hour.
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s, want the delay capped at MaxBackoff", elapsed)
	}
}

func TestDefaultRetryAfterIgnoresNonPositiveDelays(t *testing.T) {
	if _, ok := DefaultRetryAfter(errors.New("plain")); ok {
		t.Error("a plain error must not report a mandated delay")
	}
	if _, ok := DefaultRetryAfter(&retryAfterError{delay: 0}); ok {
		t.Error("a zero delay carries no information and must be ignored")
	}
	// The delay must be found through a wrapped error chain.
	if d, ok := DefaultRetryAfter(errors.Wrap(&retryAfterError{delay: time.Second}, "wrapped")); !ok || d != time.Second {
		t.Errorf("DefaultRetryAfter through a wrapped error = (%s, %v), want (1s, true)", d, ok)
	}
}

// mockClient counts calls and fails the first failUntil of them with err.
type mockClient struct {
	mu        sync.Mutex
	callN     int
	failUntil int
	err       error
}

func (m *mockClient) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.callN
}

func (m *mockClient) next() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.callN++
	return m.callN <= m.failUntil
}

func (m *mockClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	if m.next() {
		return nil, m.err
	}
	return mockEmbeddings{}, nil
}

func (m *mockClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	if m.next() {
		return nil, m.err
	}
	return llm.NewChatCompletionResponse(llm.NewMessage(llm.RoleAssistant, "ok"), nil), nil
}

func (m *mockClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	if m.next() {
		return nil, m.err
	}
	ch := make(chan llm.StreamChunk)
	close(ch)
	return ch, nil
}

func (m *mockClient) Transcription(context.Context, []byte, ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	return nil, errors.New("not implemented")
}

type mockEmbeddings struct{}

func (mockEmbeddings) Embeddings() [][]float64    { return [][]float64{{1}} }
func (mockEmbeddings) Usage() llm.EmbeddingsUsage { return nil }

var _ llm.Client = &mockClient{}
