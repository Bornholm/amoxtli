package yzma

import "github.com/hybridgroup/yzma/pkg/llama"

// Defaults sized for mxbai-embed-large (BERT, 335M parameters, 1024 dimensions,
// 512-token context), the model the backend evaluation settled on.
const (
	// DefaultMaxSequenceTokens caps a single input. It must not exceed the
	// model's training context — 512 for mxbai — and it also bounds the batch
	// budget below.
	DefaultMaxSequenceTokens = 512
	// DefaultMaxSequences is how many inputs are packed into one llama.cpp
	// batch. Batching is where most of the speed-up over Ollama comes from, and
	// eight sequences of 512 tokens is a ~4k-token batch: comfortable for an
	// encoder of this size without an unreasonable KV cache.
	DefaultMaxSequences = 8
	// DefaultGPULayers offloads the whole model. An encoder of a few hundred
	// megabytes fits on any device worth targeting, and a partial offload is
	// slower than either extreme.
	DefaultGPULayers = 999
)

type options struct {
	libraryPath       string
	gpuLayers         int32
	maxSequenceTokens int
	maxSequences      int
	threads           int32
	poolingType       llama.PoolingType
	normalize         bool
	truncate          bool
	verbose           bool
}

func newOptions(funcs ...OptionFunc) *options {
	opts := &options{
		gpuLayers:         DefaultGPULayers,
		maxSequenceTokens: DefaultMaxSequenceTokens,
		maxSequences:      DefaultMaxSequences,
		// Unspecified lets llama.cpp use the pooling the GGUF declares, which is
		// what the model was trained with (CLS for mxbai). Overriding it is
		// almost always a mistake, hence the default.
		poolingType: llama.PoolingTypeUnspecified,
		normalize:   true,
		truncate:    true,
	}

	for _, fn := range funcs {
		fn(opts)
	}

	return opts
}

// OptionFunc configures a Client.
type OptionFunc func(*options)

// WithLibraryPath points yzma at the directory holding the llama.cpp shared
// libraries. Leave it empty to use yzma's default lookup.
//
// Point it at a Vulkan build: on the reference hardware the Vulkan backend more
// than doubles the CPU throughput on a Q8_0 model (424 → 920 tokens/s).
func WithLibraryPath(path string) OptionFunc {
	return func(o *options) {
		o.libraryPath = path
	}
}

// WithGPULayers sets how many transformer layers are offloaded to the GPU.
// Zero keeps the model entirely on the CPU.
func WithGPULayers(layers int) OptionFunc {
	return func(o *options) {
		if layers >= 0 {
			o.gpuLayers = int32(layers)
		}
	}
}

// WithMaxSequenceTokens caps how many tokens of a single input are embedded.
// It must be at least the model's context size for inputs to be embedded whole;
// longer inputs are truncated (see WithTruncate).
func WithMaxSequenceTokens(tokens int) OptionFunc {
	return func(o *options) {
		if tokens > 0 {
			o.maxSequenceTokens = tokens
		}
	}
}

// WithMaxSequences sets how many inputs are packed into a single llama.cpp
// batch. Raising it trades memory for throughput.
func WithMaxSequences(sequences int) OptionFunc {
	return func(o *options) {
		if sequences > 0 {
			o.maxSequences = sequences
		}
	}
}

// WithThreads sets the number of CPU threads. Zero leaves llama.cpp's own
// default, which is generally the right answer.
func WithThreads(threads int) OptionFunc {
	return func(o *options) {
		if threads > 0 {
			o.threads = int32(threads)
		}
	}
}

// WithPoolingType overrides how token embeddings are pooled into a sequence
// embedding. The default follows the model's own declaration; setting this to
// something the model was not trained with produces vectors that are silently
// wrong rather than an error.
func WithPoolingType(pooling llama.PoolingType) OptionFunc {
	return func(o *options) {
		o.poolingType = pooling
	}
}

// WithNormalize controls L2 normalisation of the returned vectors. It is on by
// default: llama.cpp returns raw pooled vectors, whereas every embeddings
// endpoint amoxtli talks to returns normalised ones, and the vector indexes
// compare with cosine distance.
func WithNormalize(normalize bool) OptionFunc {
	return func(o *options) {
		o.normalize = normalize
	}
}

// WithTruncate controls what happens to an input longer than the sequence cap.
// On by default, matching what an embeddings server does. Turning it off makes
// an oversized input an error instead — useful to find a chunker that is
// producing chunks the model cannot represent, rather than silently embedding
// their first half.
func WithTruncate(truncate bool) OptionFunc {
	return func(o *options) {
		o.truncate = truncate
	}
}

// WithVerbose lets llama.cpp log to stderr. Off by default.
func WithVerbose(verbose bool) OptionFunc {
	return func(o *options) {
		o.verbose = verbose
	}
}
