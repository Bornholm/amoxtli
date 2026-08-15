package yzma_test

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/llmx/yzma"
)

// TestClientEmbedsAgainstRealModel exercises the whole native path — library
// load, model load, tokenisation, batched encode, pooling, normalisation.
//
// It needs a GGUF embeddings model and the llama.cpp shared libraries, so it
// skips unless told where they are:
//
//	AMOXTLI_TEST_YZMA_MODEL=/path/to/mxbai-embed-large-q8_0.gguf
//	AMOXTLI_TEST_YZMA_LIB=/path/to/llama.cpp/libs   (optional)
func TestClientEmbedsAgainstRealModel(t *testing.T) {
	modelPath := os.Getenv("AMOXTLI_TEST_YZMA_MODEL")
	if modelPath == "" {
		t.Skip("set AMOXTLI_TEST_YZMA_MODEL to a GGUF embeddings model to run")
	}

	client, err := yzma.New(modelPath,
		yzma.WithLibraryPath(os.Getenv("AMOXTLI_TEST_YZMA_LIB")),
	)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Errorf("closing client: %+v", err)
		}
	})

	ctx := context.Background()

	// More inputs than DefaultMaxSequences, so the packing runs several batches
	// and the KV cache reset between them is exercised rather than assumed.
	inputs := []string{
		"The mitochondrion is the powerhouse of the cell.",
		"Photosynthesis converts light energy into chemical energy.",
		"A cat is a small domesticated carnivorous mammal.",
		"Le chat est un petit mammifère carnivore domestique.",
		"Quantum entanglement links the states of two particles.",
		"The treaty was signed in Vienna during the spring of 1815.",
		"Enzymes lower the activation energy of biochemical reactions.",
		"Rust and Go are both statically typed compiled languages.",
		"Mitochondrial dysfunction impairs cardiac muscle contraction.",
		"The Pacific Ocean is the largest and deepest of Earth's oceans.",
	}

	res, err := client.Embeddings(ctx, inputs)
	if err != nil {
		t.Fatalf("%+v", err)
	}

	vectors := res.Embeddings()
	if len(vectors) != len(inputs) {
		t.Fatalf("got %d vectors for %d inputs", len(vectors), len(inputs))
	}

	for i, vec := range vectors {
		if len(vec) != client.Dimensions() {
			t.Fatalf("vector %d has %d dimensions, want %d", i, len(vec), client.Dimensions())
		}

		var norm float64
		for _, v := range vec {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				t.Fatalf("vector %d contains %v", i, v)
			}
			norm += v * v
		}
		// Normalisation is on by default, and the vector indexes compare with
		// cosine distance — a non-unit vector would skew every comparison.
		if math.Abs(math.Sqrt(norm)-1) > 1e-5 {
			t.Errorf("vector %d has norm %.6f, want 1", i, math.Sqrt(norm))
		}
	}

	// The real test of correctness: a batched encode must not leak state
	// between sequences. Semantically close inputs have to end up closer than
	// unrelated ones — if the KV cache were not cleared, or the sequence IDs
	// were mixed up, this is what would break.
	related := cosine(vectors[0], vectors[8])   // both mitochondria
	unrelated := cosine(vectors[0], vectors[5]) // mitochondria vs the treaty

	if related <= unrelated {
		t.Errorf("cosine(mitochondria pair) = %.4f is not above cosine(unrelated) = %.4f: the batch is leaking state between sequences",
			related, unrelated)
	}

	// The same input embedded twice, in a different batch position, must give
	// the same vector.
	again, err := client.Embeddings(ctx, []string{inputs[0]})
	if err != nil {
		t.Fatalf("%+v", err)
	}
	if got := cosine(vectors[0], again.Embeddings()[0]); got < 0.999 {
		t.Errorf("the same input embedded twice has cosine %.6f, want ~1: the result depends on batch position", got)
	}

	if usage := res.Usage(); usage.PromptTokens() <= 0 {
		t.Errorf("usage reports %d prompt tokens, want the tokens actually encoded", usage.PromptTokens())
	}
}

func TestClientRejectsOversizedInputWhenTruncationIsOff(t *testing.T) {
	modelPath := os.Getenv("AMOXTLI_TEST_YZMA_MODEL")
	if modelPath == "" {
		t.Skip("set AMOXTLI_TEST_YZMA_MODEL to a GGUF embeddings model to run")
	}

	client, err := yzma.New(modelPath,
		yzma.WithLibraryPath(os.Getenv("AMOXTLI_TEST_YZMA_LIB")),
		yzma.WithMaxSequenceTokens(16),
		yzma.WithTruncate(false),
	)
	if err != nil {
		t.Fatalf("%+v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	long := ""
	for range 100 {
		long += "word "
	}

	// The point of WithTruncate(false): surface a chunker producing chunks the
	// model cannot represent, instead of silently embedding their beginning.
	if _, err := client.Embeddings(context.Background(), []string{long}); err == nil {
		t.Error("expected an error for an input over the token cap with truncation off")
	}
}

func cosine(a, b []float64) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
