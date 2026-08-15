package yzma

import (
	"math"
	"slices"
	"testing"

	"github.com/hybridgroup/yzma/pkg/llama"
)

// These tests cover the logic that decides what llama.cpp is asked to do. They
// deliberately load no model: the interesting failure modes here — a batch
// exceeding n_ubatch, a vector that is not unit length — are decided before any
// native call, and pinning them needs neither the shared libraries nor a GGUF.
// The end-to-end path is exercised by TestClientEmbedsAgainstRealModel, which
// skips unless a model is configured.

// TestPackBatchesRespectsTokenBudget is the one that matters most: llama.cpp
// asserts n_ubatch >= n_tokens for an encoder, and a failed assert aborts the
// process instead of returning an error. No group may exceed the budget.
func TestPackBatchesRespectsTokenBudget(t *testing.T) {
	lengths := []int{300, 300, 100, 512, 12, 400}
	const maxTokens = 1024

	batches := packBatches(lengths, maxTokens, 8)

	for i, batch := range batches {
		total := 0
		for _, input := range batch {
			total += lengths[input]
		}
		if total > maxTokens {
			t.Errorf("batch %d carries %d tokens, over the %d budget", i, total, maxTokens)
		}
	}
}

func TestPackBatchesRespectsSequenceLimit(t *testing.T) {
	lengths := []int{1, 1, 1, 1, 1, 1, 1}
	const maxSequences = 3

	// The token budget is deliberately far larger than the inputs need, so only
	// the sequence limit can split them.
	for i, batch := range packBatches(lengths, 100_000, maxSequences) {
		if len(batch) > maxSequences {
			t.Errorf("batch %d holds %d sequences, over the %d limit", i, len(batch), maxSequences)
		}
	}
}

// TestPackBatchesCoversEveryInputOnce guards the mapping back to the caller's
// slice: a dropped or duplicated index would silently return the wrong vector
// for an input.
func TestPackBatchesCoversEveryInputOnce(t *testing.T) {
	lengths := []int{120, 480, 33, 500, 7, 260, 199}

	var seen []int
	for _, batch := range packBatches(lengths, 512, 4) {
		seen = append(seen, batch...)
	}

	slices.Sort(seen)
	for i := range lengths {
		if i >= len(seen) || seen[i] != i {
			t.Fatalf("packed indexes = %v, want each of 0..%d exactly once", seen, len(lengths)-1)
		}
	}
}

// An input at exactly the budget must still be packed, alone, rather than
// dropped or looped on forever.
func TestPackBatchesHandlesAnInputFillingTheBudget(t *testing.T) {
	batches := packBatches([]int{512, 10}, 512, 8)

	if len(batches) != 2 {
		t.Fatalf("packed %v, want the full-budget input in a batch of its own", batches)
	}
	if len(batches[0]) != 1 || batches[0][0] != 0 {
		t.Errorf("first batch = %v, want just input 0", batches[0])
	}
}

func TestPackBatchesHandlesNoInput(t *testing.T) {
	if got := packBatches(nil, 512, 8); len(got) != 0 {
		t.Errorf("packBatches(nil) = %v, want no batch", got)
	}
}

func TestNormalizeL2ProducesUnitVectors(t *testing.T) {
	vec := []float64{3, 4}
	normalizeL2(vec)

	if math.Abs(vec[0]-0.6) > 1e-12 || math.Abs(vec[1]-0.8) > 1e-12 {
		t.Errorf("normalized = %v, want [0.6 0.8]", vec)
	}

	var norm float64
	for _, v := range vec {
		norm += v * v
	}
	if math.Abs(math.Sqrt(norm)-1) > 1e-12 {
		t.Errorf("norm = %.15f, want 1", math.Sqrt(norm))
	}
}

// A zero vector has no direction: normalising it would divide by zero and fill
// the vector with NaNs, which then poison every cosine comparison downstream
// without ever raising an error.
func TestNormalizeL2LeavesZeroVectorAlone(t *testing.T) {
	vec := []float64{0, 0, 0}
	normalizeL2(vec)

	for i, v := range vec {
		if math.IsNaN(v) || v != 0 {
			t.Errorf("normalized[%d] = %v, want 0", i, v)
		}
	}
}

func TestDefaultOptionsFollowTheModel(t *testing.T) {
	opts := newOptions()

	// Overriding pooling with anything the model was not trained with yields
	// vectors that are wrong without being detectably wrong, so the default has
	// to defer to the GGUF.
	if opts.poolingType != llama.PoolingTypeUnspecified {
		t.Errorf("default pooling = %v, want Unspecified so the model decides", opts.poolingType)
	}
	if !opts.normalize {
		t.Error("normalization is off by default; the vector indexes compare with cosine distance")
	}
	if !opts.truncate {
		t.Error("truncation is off by default; an oversized input would then be an error rather than a shortened vector")
	}
}

func TestOptionsRejectNonsense(t *testing.T) {
	opts := newOptions(
		WithMaxSequenceTokens(0),
		WithMaxSequences(-1),
		WithThreads(-4),
		WithGPULayers(-1),
	)

	if opts.maxSequenceTokens != DefaultMaxSequenceTokens {
		t.Errorf("maxSequenceTokens = %d, want the default %d", opts.maxSequenceTokens, DefaultMaxSequenceTokens)
	}
	if opts.maxSequences != DefaultMaxSequences {
		t.Errorf("maxSequences = %d, want the default %d", opts.maxSequences, DefaultMaxSequences)
	}
	if opts.threads != 0 {
		t.Errorf("threads = %d, want 0 so llama.cpp picks", opts.threads)
	}
	if opts.gpuLayers != DefaultGPULayers {
		t.Errorf("gpuLayers = %d, want the default %d", opts.gpuLayers, DefaultGPULayers)
	}
}

// The context is sized from the options, and that sizing is what keeps
// packBatches within n_ubatch. Pin the arithmetic the two share.
func TestBatchBudgetMatchesContextSizing(t *testing.T) {
	opts := newOptions(WithMaxSequenceTokens(512), WithMaxSequences(8))

	budget := opts.maxSequences * opts.maxSequenceTokens

	// The worst case packBatches can produce: maxSequences inputs, each at the
	// per-sequence cap.
	lengths := make([]int, opts.maxSequences)
	for i := range lengths {
		lengths[i] = opts.maxSequenceTokens
	}

	batches := packBatches(lengths, budget, opts.maxSequences)
	if len(batches) != 1 {
		t.Fatalf("packed %d batches, want the worst case to fit in one", len(batches))
	}

	total := 0
	for _, input := range batches[0] {
		total += lengths[input]
	}
	if total != budget {
		t.Errorf("worst-case batch = %d tokens, context sized for %d", total, budget)
	}
}
