package llmx

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/bornholm/genai/llm"
)

// TestCachingClientCollapsesConcurrentEmbeddingMisses covers the gap the
// on-disk cache alone leaves open: the entry is only written once the call has
// returned, so callers arriving during the call all miss and, without
// singleflight, all pay for their own round-trip.
func TestCachingClientCollapsesConcurrentEmbeddingMisses(t *testing.T) {
	const callers = 20

	release := make(chan struct{})
	e := &blockingEmbedder{release: release}
	c := newTestCache(t, e)

	var wg sync.WaitGroup
	results := make([]llm.EmbeddingsResponse, callers)
	errs := make([]error, callers)

	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = c.Embeddings(context.Background(), []string{"alpha"})
		}(i)
	}

	// Every caller is now blocked behind the single in-flight call, which we
	// only let return once they have all had the chance to join it.
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: unexpected error: %+v", i, err)
		}
	}

	if got := e.calls(); got != 1 {
		t.Errorf("inner calls = %d, want 1 (%d concurrent identical misses must collapse)", got, callers)
	}

	want := [][]float64{vectorFor("alpha")}
	for i, res := range results {
		if !reflect.DeepEqual(res.Embeddings(), want) {
			t.Errorf("caller %d: embeddings = %v, want %v", i, res.Embeddings(), want)
		}
	}
}

// TestCachingClientDoesNotShareVectorBackingArrays guards the deduplicated
// callers against each other: they were served from one response, but a caller
// normalising its vectors in place must not corrupt its neighbours'.
func TestCachingClientDoesNotShareVectorBackingArrays(t *testing.T) {
	const callers = 8

	release := make(chan struct{})
	e := &blockingEmbedder{release: release}
	c := newTestCache(t, e)

	var wg sync.WaitGroup
	vectors := make([][]float64, callers)

	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.Embeddings(context.Background(), []string{"alpha"})
			if err != nil {
				return
			}
			vectors[i] = res.Embeddings()[0]
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	vectors[0][0] = -42

	for i, vec := range vectors[1:] {
		if vec[0] == -42 {
			t.Fatalf("caller %d shares its vector backing array with caller 0", i+1)
		}
	}
}

// TestCachingClientCollapsesConcurrentChatMisses is the chat-side equivalent:
// several searches issuing the same seeded HyDE or judge prompt at once must
// cost one completion, not one each.
func TestCachingClientCollapsesConcurrentChatMisses(t *testing.T) {
	const callers = 16

	release := make(chan struct{})
	inner := &blockingChatStub{release: release}
	c := newChatTestCache(t, inner)

	var wg sync.WaitGroup
	answers := make([]string, callers)

	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := c.ChatCompletion(context.Background(), seeded(7)...)
			if err != nil {
				t.Errorf("caller %d: unexpected error: %+v", i, err)
				return
			}
			answers[i] = res.Message().Content()
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := inner.calls(); got != 1 {
		t.Errorf("inner calls = %d, want 1 (%d concurrent identical prompts must collapse)", got, callers)
	}

	// One call means one answer: the stub numbers its responses, so a second
	// call would surface here as a diverging answer.
	for i, answer := range answers {
		if answer != answers[0] {
			t.Errorf("caller %d answered %q, want %q", i, answer, answers[0])
		}
	}

	// The deduplicated callers are hits, not misses: only the one that issued
	// the call paid for it.
	if hits, misses := c.ChatStats(); misses != 1 || hits != callers-1 {
		t.Errorf("chat stats = %d hits / %d misses, want %d / 1", hits, misses, callers-1)
	}
}

// blockingEmbedder holds every call until release is closed, so a test can line
// up an arbitrary number of concurrent callers behind one in-flight request.
type blockingEmbedder struct {
	llm.Client

	release <-chan struct{}

	mu    sync.Mutex
	count int
}

func (e *blockingEmbedder) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count
}

func (e *blockingEmbedder) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	e.mu.Lock()
	e.count++
	e.mu.Unlock()

	<-e.release

	vecs := make([][]float64, len(inputs))
	for i, input := range inputs {
		vecs[i] = vectorFor(input)
	}

	return stubEmbeddingsResponse{vecs: vecs}, nil
}

// blockingChatStub is the chat counterpart of blockingEmbedder.
type blockingChatStub struct {
	llm.Client

	release <-chan struct{}

	mu    sync.Mutex
	count int
}

func (s *blockingChatStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *blockingChatStub) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	s.mu.Lock()
	s.count++
	n := s.count
	s.mu.Unlock()

	<-s.release

	return llm.NewChatCompletionResponse(
		llm.NewMessage(llm.RoleAssistant, "answer #"+string(rune('0'+n))),
		llm.NewChatCompletionUsage(10, 5, 15),
	), nil
}
