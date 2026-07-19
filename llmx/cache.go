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
// Embeddings calls and, optionally (WithChatCache), for deterministic
// ChatCompletion calls. Embedding a text is deterministic for a given model,
// so the vector can be reused across runs — turning repeated corpus ingestions
// (evaluation re-runs, benchmarks) into free cache hits instead of thousands
// of billable, rate-limited calls. A chat completion is only cacheable when
// the caller pins a seed (e.g. the HyDE transformer, seeded per query for
// reproducibility): unseeded, tool-using or multimodal calls always pass
// through.
//
// Each embeddings input is cached individually under sha256(namespace,
// dimensions, text) below dir/embeddings, so hits survive re-batching. On a
// batch call only the misses are forwarded to the wrapped client (in one
// batch), and the reported usage covers those misses only. Chat completions
// are cached below dir/chat under a key covering the messages, seed,
// temperature and response schema. Files are written atomically (temp file +
// rename) making the cache safe for concurrent use; an unreadable or corrupted
// entry is treated as a miss and rewritten.
//
// The namespace must identify the embedding space — typically the model name.
// Reusing a directory with a different model but the same namespace would serve
// vectors from the wrong space. The chat namespace (WithChatCache) must
// likewise identify the chat model.
type CachingClient struct {
	inner     llm.Client
	dir       string
	namespace string

	chatCache     bool
	chatNamespace string

	hits   atomic.Int64
	misses atomic.Int64

	chatHits   atomic.Int64
	chatMisses atomic.Int64
}

// CachingOption configures a CachingClient.
type CachingOption func(*CachingClient)

// WithChatCache enables caching of deterministic (seeded) chat completions
// below dir/chat, keyed under namespace — which must identify the chat model,
// exactly like the embeddings namespace identifies the embedding space.
func WithChatCache(namespace string) CachingOption {
	return func(c *CachingClient) {
		c.chatCache = true
		c.chatNamespace = namespace
	}
}

// NewCachingClient wraps client with an embeddings cache rooted at dir (created
// if missing), keyed under namespace (typically the embedding model name).
func NewCachingClient(client llm.Client, dir, namespace string, funcs ...CachingOption) (*CachingClient, error) {
	if dir == "" {
		return nil, errors.New("llmx: cache directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, errors.WithStack(err)
	}
	c := &CachingClient{
		inner:     client,
		dir:       dir,
		namespace: namespace,
	}
	for _, fn := range funcs {
		fn(c)
	}
	return c, nil
}

// Stats returns the number of embeddings cache hits and misses served so far.
func (c *CachingClient) Stats() (hits, misses int64) {
	return c.hits.Load(), c.misses.Load()
}

// ChatStats returns the number of chat-completion cache hits and misses served
// so far (both stay 0 unless WithChatCache is enabled).
func (c *CachingClient) ChatStats() (hits, misses int64) {
	return c.chatHits.Load(), c.chatMisses.Load()
}

// chatCacheEntry is the persisted form of a cached chat completion.
type chatCacheEntry struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatKey is the canonical, JSON-marshaled identity of a cacheable chat
// completion. encoding/json sorts map keys, so schemas built from maps
// marshal deterministically.
type chatKey struct {
	Namespace           string           `json:"namespace"`
	Seed                int              `json:"seed"`
	Temperature         float64          `json:"temperature"`
	MaxCompletionTokens *int             `json:"max_completion_tokens,omitempty"`
	ResponseFormat      string           `json:"response_format,omitempty"`
	SchemaName          string           `json:"schema_name,omitempty"`
	SchemaDescription   string           `json:"schema_description,omitempty"`
	Schema              any              `json:"schema,omitempty"`
	Messages            []chatCacheEntry `json:"messages"`
}

// ChatCompletion implements llm.ChatCompletionClient. Deterministic calls —
// seeded, tool-free, text-only — are served from the chat cache when enabled;
// everything else is delegated untouched.
func (c *CachingClient) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	if !c.chatCache {
		return c.inner.ChatCompletion(ctx, funcs...)
	}

	opts := llm.NewChatCompletionOptions(funcs...)

	path, cacheable := c.chatPath(opts)
	if !cacheable {
		return c.inner.ChatCompletion(ctx, funcs...)
	}

	if entry, ok := c.loadChat(path); ok {
		c.chatHits.Add(1)
		return llm.NewChatCompletionResponse(
			llm.NewMessage(llm.Role(entry.Role), entry.Content),
			llm.NewChatCompletionUsage(0, 0, 0),
		), nil
	}
	c.chatMisses.Add(1)

	res, err := c.inner.ChatCompletion(ctx, funcs...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	// Only plain text responses are cached: a response carrying tool calls (or
	// no message at all) is returned as-is and recomputed next time.
	if msg := res.Message(); msg != nil && len(res.ToolCalls()) == 0 && msg.Content() != "" {
		entry := chatCacheEntry{Role: string(msg.Role()), Content: msg.Content()}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if err := c.storeBytes(path, data); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return res, nil
}

// chatPath derives the cache file path of a chat completion, reporting whether
// the call is cacheable at all: it must be seeded (the determinism opt-in),
// tool-free and text-only.
func (c *CachingClient) chatPath(opts *llm.ChatCompletionOptions) (string, bool) {
	if opts.Seed == nil || len(opts.Tools) > 0 {
		return "", false
	}

	key := chatKey{
		Namespace:           c.chatNamespace,
		Seed:                *opts.Seed,
		Temperature:         opts.Temperature,
		MaxCompletionTokens: opts.MaxCompletionTokens,
		ResponseFormat:      string(opts.ResponseFormat),
	}

	if opts.ResponseSchema != nil {
		key.SchemaName = opts.ResponseSchema.Name()
		key.SchemaDescription = opts.ResponseSchema.Description()
		key.Schema = opts.ResponseSchema.Schema()
	}

	for _, msg := range opts.Messages {
		if len(msg.Attachments()) > 0 {
			return "", false
		}
		key.Messages = append(key.Messages, chatCacheEntry{Role: string(msg.Role()), Content: msg.Content()})
	}

	data, err := json.Marshal(key)
	if err != nil {
		return "", false
	}

	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])

	return filepath.Join(c.dir, "chat", digest[:2], digest+".json"), true
}

// loadChat reads a cached chat completion; any error (missing, corrupted) is a
// miss.
func (c *CachingClient) loadChat(path string) (*chatCacheEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var entry chatCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil || entry.Content == "" {
		return nil, false
	}
	return &entry, true
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

// path derives the cache file path for one embeddings input: sha256 over the
// namespace, the requested dimensions and the text, sharded on the first hex
// byte below dir/embeddings.
func (c *CachingClient) path(input string, opts *llm.EmbeddingsOptions) string {
	dims := ""
	if opts.Dimensions != nil {
		dims = strconv.Itoa(*opts.Dimensions)
	}

	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00", c.namespace, dims)
	h.Write([]byte(input))
	key := hex.EncodeToString(h.Sum(nil))

	return filepath.Join(c.dir, "embeddings", key[:2], key+".json")
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
	data, err := json.Marshal(vec)
	if err != nil {
		return errors.WithStack(err)
	}
	return c.storeBytes(path, data)
}

// storeBytes writes a cache entry atomically (temp file + rename), so
// concurrent readers never observe a partial entry.
func (c *CachingClient) storeBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errors.WithStack(err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return errors.WithStack(err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return errors.WithStack(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return errors.WithStack(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
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
