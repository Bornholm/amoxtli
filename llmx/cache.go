package llmx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"

	"github.com/bornholm/genai/llm"
	"github.com/pkg/errors"
)

// CachingClient decorates an llm.Client with a persistent on-disk cache for
// Embeddings calls; chat completion calls pass through untouched. Embedding a
// text is deterministic for a given model, so the vector can be reused across
// runs — turning repeated corpus ingestions (evaluation re-runs, benchmarks)
// into free cache hits instead of thousands of billable, rate-limited calls.
//
// Each input is cached individually under sha256(namespace, dimensions, text),
// so hits survive re-batching. On a batch call only the misses are forwarded to
// the wrapped client (in one batch), and the reported usage covers those misses
// only. Files are written atomically (temp file + rename) making the cache safe
// for concurrent use; an unreadable or corrupted entry is treated as a miss and
// rewritten.
//
// The namespace must identify the embedding space — typically the model name.
// Reusing a directory with a different model but the same namespace would serve
// vectors from the wrong space.
type CachingClient struct {
	inner     llm.Client
	dir       string
	namespace string

	hits   atomic.Int64
	misses atomic.Int64
}

// NewCachingClient wraps client with an embeddings cache rooted at dir (created
// if missing), keyed under namespace (typically the embedding model name).
func NewCachingClient(client llm.Client, dir, namespace string) (*CachingClient, error) {
	if dir == "" {
		return nil, errors.New("llmx: cache directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errors.WithStack(err)
	}
	return &CachingClient{
		inner:     client,
		dir:       dir,
		namespace: namespace,
	}, nil
}

// Stats returns the number of cache hits and misses served so far.
func (c *CachingClient) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// ChatCompletion implements llm.ChatCompletionClient by delegation (not cached:
// chat completions are not deterministic).
func (c *CachingClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return c.inner.ChatCompletion(ctx, funcs...)
}

// ChatCompletionStream implements llm.ChatCompletionStreamingClient by
// delegation.
func (c *CachingClient) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return c.inner.ChatCompletionStream(ctx, funcs...)
}

// Embeddings implements llm.EmbeddingsClient: served from the cache when every
// input is known, otherwise the misses are fetched from the wrapped client in a
// single batch and stored before assembling the response in input order.
func (c *CachingClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	opts := llm.NewEmbeddingsOptions(funcs...)

	embeddings := make([][]float64, len(inputs))
	missIndexes := make([]int, 0, len(inputs))
	missInputs := make([]string, 0, len(inputs))

	for i, input := range inputs {
		if vec, ok := c.load(c.path(input, opts)); ok {
			embeddings[i] = vec
			continue
		}
		missIndexes = append(missIndexes, i)
		missInputs = append(missInputs, input)
	}
	c.hits.Add(int64(len(inputs) - len(missInputs)))
	c.misses.Add(int64(len(missInputs)))

	usage := llm.NewEmbeddingsUsage(0, 0)
	if len(missInputs) > 0 {
		res, err := c.inner.Embeddings(ctx, missInputs, funcs...)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		fetched := res.Embeddings()
		if len(fetched) != len(missInputs) {
			return nil, errors.Errorf("llmx: embeddings response has %d vectors for %d inputs", len(fetched), len(missInputs))
		}

		for j, i := range missIndexes {
			embeddings[i] = fetched[j]
			if err := c.store(c.path(missInputs[j], opts), fetched[j]); err != nil {
				return nil, errors.WithStack(err)
			}
		}

		if u := res.Usage(); u != nil {
			usage = llm.NewEmbeddingsUsage(u.PromptTokens(), u.TotalTokens())
		}
	}

	return &cachedEmbeddingsResponse{embeddings: embeddings, usage: usage}, nil
}

// path derives the cache file path for one input: sha256 over the namespace,
// the requested dimensions and the text, sharded on the first hex byte.
func (c *CachingClient) path(input string, opts *llm.EmbeddingsOptions) string {
	dims := ""
	if opts.Dimensions != nil {
		dims = strconv.Itoa(*opts.Dimensions)
	}

	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00", c.namespace, dims)
	h.Write([]byte(input))
	key := hex.EncodeToString(h.Sum(nil))

	return filepath.Join(c.dir, key[:2], key+".json")
}

// load reads a cached vector; any error (missing, corrupted) is a miss.
func (c *CachingClient) load(path string) ([]float64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var vec []float64
	if err := json.Unmarshal(data, &vec); err != nil || len(vec) == 0 {
		return nil, false
	}
	return vec, true
}

// store writes a vector atomically (temp file + rename), so concurrent readers
// never observe a partial entry.
func (c *CachingClient) store(path string, vec []float64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errors.WithStack(err)
	}

	data, err := json.Marshal(vec)
	if err != nil {
		return errors.WithStack(err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return errors.WithStack(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return errors.WithStack(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return errors.WithStack(err)
	}

	return nil
}

// cachedEmbeddingsResponse assembles cached and freshly fetched vectors; usage
// reflects only the tokens actually billed by the wrapped client.
type cachedEmbeddingsResponse struct {
	embeddings [][]float64
	usage      llm.EmbeddingsUsage
}

func (r *cachedEmbeddingsResponse) Embeddings() [][]float64    { return r.embeddings }
func (r *cachedEmbeddingsResponse) Usage() llm.EmbeddingsUsage { return r.usage }

var _ llm.Client = &CachingClient{}
var _ llm.EmbeddingsResponse = &cachedEmbeddingsResponse{}
