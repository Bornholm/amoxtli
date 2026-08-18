package yzma

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"

	"github.com/bornholm/genai/llm"
	"github.com/hybridgroup/yzma/pkg/llama"
	"github.com/pkg/errors"
)

// Client is an embeddings-only [llm.Client] running the model in-process
// through llama.cpp.
//
// It is safe for concurrent use — amoxtli's vector indexes embed chunks from an
// errgroup — but the calls are serialised: a llama.cpp context holds mutable
// compute buffers and a KV cache, and is not reentrant. Concurrency therefore
// buys nothing here; throughput comes from batching inside a single call, which
// is why Embeddings takes the whole input slice and packs it.
type Client struct {
	opts  *options
	model llama.Model
	lctx  llama.Context
	vocab llama.Vocab

	nEmbd int32
	// encoder records whether the model is encoder-only (BERT and friends), in
	// which case llama.cpp wants llama_encode rather than llama_decode.
	encoder bool
	// batchTokens is the physical batch size the context was created with, and
	// therefore the hard cap on the tokens a single Encode may carry.
	batchTokens int

	mu    sync.Mutex
	batch llama.Batch

	promptTokens atomic.Int64
	closed       atomic.Bool
}

// New loads a GGUF embeddings model and prepares a context for it.
//
// The returned Client owns the model and the llama.cpp context; call Close to
// release them. The process-global llama.cpp backend outlives it — see
// [Shutdown].
func New(modelPath string, funcs ...OptionFunc) (*Client, error) {
	opts := newOptions(funcs...)

	if err := loadRuntime(opts.libraryPath, opts.verbose); err != nil {
		return nil, errors.WithStack(err)
	}

	modelParams := llama.ModelDefaultParams()
	modelParams.NGpuLayers = opts.gpuLayers

	model, err := llama.ModelLoadFromFile(modelPath, modelParams)
	if err != nil {
		return nil, errors.Wrapf(err, "yzma: loading model %q", modelPath)
	}
	if model == 0 {
		return nil, errors.Errorf("yzma: loading model %q returned no model", modelPath)
	}

	// The batch must fit every sequence it carries, all at once: llama.cpp
	// asserts n_ubatch >= n_tokens for an encoder, and an assert aborts the
	// process rather than returning an error. Sizing the physical batch at
	// maxSequences * maxSequenceTokens is what makes that unreachable, and the
	// packing in Embeddings never exceeds it.
	batchTokens := opts.maxSequences * opts.maxSequenceTokens

	ctxParams := llama.ContextDefaultParams()
	ctxParams.Embeddings = 1
	ctxParams.PoolingType = opts.poolingType
	ctxParams.NCtx = uint32(batchTokens)
	ctxParams.NBatch = uint32(batchTokens)
	ctxParams.NUbatch = uint32(batchTokens)
	ctxParams.NSeqMax = uint32(opts.maxSequences)
	if opts.threads > 0 {
		ctxParams.NThreads = opts.threads
		ctxParams.NThreadsBatch = opts.threads
	}

	lctx, err := llama.InitFromModel(model, ctxParams)
	if err != nil {
		_ = llama.ModelFree(model)
		return nil, errors.Wrapf(err, "yzma: initializing context for model %q", modelPath)
	}

	client := &Client{
		opts:        opts,
		model:       model,
		lctx:        lctx,
		vocab:       llama.ModelGetVocab(model),
		nEmbd:       llama.ModelNEmbd(model),
		encoder:     llama.ModelHasEncoder(model),
		batchTokens: batchTokens,
		batch:       llama.BatchInit(int32(batchTokens), 0, int32(opts.maxSequences)),
	}

	if client.nEmbd <= 0 {
		_ = client.Close()
		return nil, errors.Errorf("yzma: model %q reports %d embedding dimensions", modelPath, client.nEmbd)
	}

	slog.Debug("yzma embeddings client ready",
		slog.String("model", modelPath),
		slog.Int("dimensions", int(client.nEmbd)),
		slog.Bool("encoder", client.encoder),
		slog.Int("batchTokens", batchTokens),
	)

	return client, nil
}

// Dimensions returns the model's embedding size.
func (c *Client) Dimensions() int { return int(c.nEmbd) }

// Embeddings implements [llm.EmbeddingsClient].
func (c *Client) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	if c.closed.Load() {
		return nil, errors.New("yzma: client is closed")
	}
	if len(inputs) == 0 {
		return &response{usage: llm.NewEmbeddingsUsage(0, 0)}, nil
	}

	opts := llm.NewEmbeddingsOptions(funcs...)
	if opts.Dimensions != nil && *opts.Dimensions != int(c.nEmbd) {
		// Truncating the vector would produce something that looks like an
		// embedding and compares like noise, unless the model was trained for
		// it (Matryoshka). Refusing is the only safe answer.
		return nil, errors.Errorf("yzma: model produces %d dimensions, %d requested", c.nEmbd, *opts.Dimensions)
	}

	tokenized := make([][]llama.Token, len(inputs))
	var promptTokens int64

	for i, input := range inputs {
		tokens := llama.Tokenize(c.vocab, input, true, true)

		if len(tokens) > c.opts.maxSequenceTokens {
			if !c.opts.truncate {
				return nil, errors.Errorf("yzma: input %d is %d tokens, over the %d-token cap", i, len(tokens), c.opts.maxSequenceTokens)
			}
			slog.WarnContext(ctx, "yzma: truncating oversized input",
				slog.Int("input", i),
				slog.Int("tokens", len(tokens)),
				slog.Int("cap", c.opts.maxSequenceTokens),
			)
			tokens = tokens[:c.opts.maxSequenceTokens]
		}

		if len(tokens) == 0 {
			// llama.cpp has nothing to encode for an empty sequence, and asking
			// it for one returns a null pointer rather than a zero vector.
			return nil, errors.Errorf("yzma: input %d tokenized to nothing", i)
		}

		tokenized[i] = tokens
		promptTokens += int64(len(tokens))
	}

	vectors := make([][]float64, len(inputs))

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, group := range packBatches(tokenLengths(tokenized), c.batchTokens, c.opts.maxSequences) {
		if err := ctx.Err(); err != nil {
			return nil, errors.WithStack(err)
		}

		if err := c.embedBatch(tokenized, group, vectors); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	c.promptTokens.Add(promptTokens)

	return &response{
		vectors: vectors,
		usage:   llm.NewEmbeddingsUsage(promptTokens, promptTokens),
	}, nil
}

// embedBatch encodes one group of inputs as a single llama.cpp batch and writes
// their vectors into out at the inputs' own indexes.
func (c *Client) embedBatch(tokenized [][]llama.Token, group []int, out [][]float64) error {
	if err := c.batch.Clear(); err != nil {
		return errors.WithStack(err)
	}

	// When the context has a KV cache, it still holds the previous batch's
	// sequences under the sequence IDs this one is about to reuse, and those
	// tokens would be appended to them instead of starting fresh.
	//
	// An encoder with pooling has no cache at all — attention is non-causal and
	// nothing is carried between batches — and llama.cpp then returns a null
	// memory handle. That is the normal case for an embeddings model, not a
	// failure, so there is simply nothing to clear.
	memory, err := llama.GetMemory(c.lctx)
	if err != nil {
		return errors.WithStack(err)
	}
	if memory != 0 {
		if err := llama.MemoryClear(memory, true); err != nil {
			return errors.WithStack(err)
		}
	}

	for slot, input := range group {
		for pos, token := range tokenized[input] {
			// Every token is marked as an output: pooling reduces the sequence
			// afterwards, but llama.cpp only pools over tokens it was asked to
			// compute.
			if err := c.batch.Add(token, llama.Pos(pos), []llama.SeqId{llama.SeqId(slot)}, true); err != nil {
				return errors.WithStack(err)
			}
		}
	}

	if err := c.run(); err != nil {
		return errors.WithStack(err)
	}

	for slot, input := range group {
		vec, err := llama.GetEmbeddingsSeq(c.lctx, llama.SeqId(slot), c.nEmbd)
		if err != nil {
			return errors.WithStack(err)
		}
		if vec == nil {
			return errors.Errorf("yzma: no embedding for sequence %d — is the model an embeddings model with a pooling type?", slot)
		}

		// vec aliases llama.cpp's own output buffer, which the next batch
		// overwrites. Convert into freshly allocated Go memory now.
		converted := make([]float64, len(vec))
		for i, v := range vec {
			converted[i] = float64(v)
		}

		if c.opts.normalize {
			normalizeL2(converted)
		}

		out[input] = converted
	}

	return nil
}

// run submits the prepared batch, using the call that matches the model's
// architecture.
func (c *Client) run() error {
	var (
		ret int32
		err error
	)

	if c.encoder {
		ret, err = llama.Encode(c.lctx, c.batch)
	} else {
		ret, err = llama.Decode(c.lctx, c.batch)
	}

	if err != nil {
		return errors.WithStack(err)
	}
	if ret != 0 {
		return errors.Errorf("yzma: llama.cpp returned %d for a batch of %d tokens", ret, c.batch.NTokens)
	}

	return nil
}

// Close releases the model and its context. It is idempotent.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := llama.BatchFree(c.batch); err != nil {
		return errors.WithStack(err)
	}
	if c.lctx != 0 {
		if err := llama.Free(c.lctx); err != nil {
			return errors.WithStack(err)
		}
	}
	if c.model != 0 {
		if err := llama.ModelFree(c.model); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// ChatCompletion implements [llm.ChatCompletionClient]. This client only serves
// embeddings; it reports so rather than satisfying the interface with a nil
// that would panic at the first call.
func (c *Client) ChatCompletion(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return nil, errors.New("yzma: this client only provides embeddings, not chat completion")
}

// ChatCompletionStream implements [llm.ChatCompletionStreamingClient].
func (c *Client) ChatCompletionStream(ctx context.Context, funcs ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("yzma: this client only provides embeddings, not chat completion")
}

// Transcription implements [llm.TranscriptionClient].
func (c *Client) Transcription(ctx context.Context, audio []byte, funcs ...llm.TranscriptionOptionFunc) (llm.TranscriptionResponse, error) {
	return nil, errors.New("yzma: this client only provides embeddings, not transcription")
}

var _ llm.Client = &Client{}

// packBatches groups input indexes into batches that respect both the token
// budget of a llama.cpp batch and its maximum number of sequences.
//
// It is greedy and order-preserving: inputs stay in their own order, which keeps
// the mapping back to the caller's slice trivial and the batching deterministic.
// An input longer than maxTokens on its own would loop forever, so callers cap
// input length before getting here; the guard below makes that failure loud
// instead.
func packBatches(lengths []int, maxTokens, maxSequences int) [][]int {
	var (
		batches [][]int
		current []int
		tokens  int
	)

	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
			tokens = 0
		}
	}

	for i, length := range lengths {
		if len(current) > 0 && (tokens+length > maxTokens || len(current) >= maxSequences) {
			flush()
		}

		current = append(current, i)
		tokens += length
	}

	flush()

	return batches
}

func tokenLengths(tokenized [][]llama.Token) []int {
	lengths := make([]int, len(tokenized))
	for i, tokens := range tokenized {
		lengths[i] = len(tokens)
	}
	return lengths
}

// normalizeL2 scales a vector to unit length, in place. A zero vector is left
// as it is: there is no direction to preserve, and dividing would produce NaNs
// that silently poison every later cosine comparison.
func normalizeL2(vec []float64) {
	var sum float64
	for _, v := range vec {
		sum += v * v
	}

	if sum == 0 {
		return
	}

	norm := math.Sqrt(sum)
	for i := range vec {
		vec[i] /= norm
	}
}

// response implements llm.EmbeddingsResponse.
type response struct {
	vectors [][]float64
	usage   llm.EmbeddingsUsage
}

func (r *response) Embeddings() [][]float64    { return r.vectors }
func (r *response) Usage() llm.EmbeddingsUsage { return r.usage }

var _ llm.EmbeddingsResponse = &response{}
