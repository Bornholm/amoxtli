package eval_test

import (
	"context"
	"os"
	"testing"

	"github.com/bornholm/amoxtli/eval"
	"github.com/bornholm/amoxtli/eval/beir"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	"github.com/pkg/errors"
)

// TestEvaluateBEIR runs the retrieval stack against a BEIR-format dataset — the
// standard IR benchmark. Several of its datasets (FiQA, SciFact, ArguAna, Quora)
// exhibit a real query↔document vocabulary gap, where semantic (dense) retrieval
// is expected to help over lexical BM25 — unlike the lexically-friendly
// SQuAD-style QA measured by TestEvaluateRealWorld.
//
// Gated behind AMOXTLI_EVAL=1. Point it at the three BEIR files:
//
//	AMOXTLI_EVAL_BEIR_CORPUS=/path/corpus.jsonl
//	AMOXTLI_EVAL_BEIR_QUERIES=/path/queries.jsonl
//	AMOXTLI_EVAL_BEIR_QRELS=/path/qrels/test.tsv
//	AMOXTLI_EVAL_BEIR_NAME=scifact            (label; namespaces the sources)
//
// Cost bounds use a gold-aware subsample, so every kept query stays answerable:
//
//	AMOXTLI_EVAL_SAMPLE_QUERIES=150
//	AMOXTLI_EVAL_SAMPLE_DOCS=500
//
// The workdir, RRF weights, embeddings and reranking knobs are identical to
// TestEvaluateRealWorld (see its doc comment).
func TestEvaluateBEIR(t *testing.T) {
	if os.Getenv("AMOXTLI_EVAL") == "" {
		t.Skip("set AMOXTLI_EVAL=1 to run the BEIR evaluation benchmark")
	}
	corpusPath := os.Getenv("AMOXTLI_EVAL_BEIR_CORPUS")
	queriesPath := os.Getenv("AMOXTLI_EVAL_BEIR_QUERIES")
	qrelsPath := os.Getenv("AMOXTLI_EVAL_BEIR_QRELS")
	if corpusPath == "" || queriesPath == "" || qrelsPath == "" {
		t.Skip("set AMOXTLI_EVAL_BEIR_CORPUS, _QUERIES and _QRELS to run")
	}

	// The gorm SQLite store and sqlite-vec both provide a WASM build to
	// ncruces/go-sqlite3; force the vec0-enabled one before any connection opens.
	sqlitevecIndex.EnsureVecWASM()

	ctx := context.Background()
	name := os.Getenv("AMOXTLI_EVAL_BEIR_NAME")
	if name == "" {
		name = "beir"
	}

	corpus, dataset, err := beir.Load(corpusPath, queriesPath, qrelsPath, name, os.Getenv("AMOXTLI_EVAL_BEIR_LANG"))
	if err != nil {
		t.Fatalf("load BEIR: %+v", errors.WithStack(err))
	}
	t.Logf("loaded BEIR %q: %d documents, %d queries (full)", name, len(corpus.Documents), len(dataset.Queries))

	sampleQueries := envInt(t, "AMOXTLI_EVAL_SAMPLE_QUERIES", 0)
	sampleDocs := envInt(t, "AMOXTLI_EVAL_SAMPLE_DOCS", 0)
	if sampleQueries > 0 || sampleDocs > 0 {
		corpus, dataset = eval.Subsample(corpus, dataset, sampleQueries, sampleDocs)
		t.Logf("subsampled to %d documents, %d queries", len(corpus.Documents), len(dataset.Queries))
	}

	topK := envInt(t, "AMOXTLI_EVAL_TOPK", 10)
	evaluateCorpus(t, ctx, corpus, dataset, topK)
}
