package eval_test

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bornholm/amoxtli"
	"github.com/bornholm/amoxtli/eval"
	"github.com/bornholm/amoxtli/index"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// topicEmbeddings maps a topic keyword to a basis vector, so a query about a
// topic lands closest to the documents about it. It replaces a live embeddings
// model: the point of this evaluation is to compare two filtering paths over
// identical rankings, not to measure an embedding model.
var topics = []string{"pastry", "grilling", "fermentation", "preserving"}

func topicEmbeddings(text string) []float64 {
	vector := make([]float64, len(topics))
	for i := range vector {
		vector[i] = 0.01
	}

	lowered := strings.ToLower(text)
	for i, topic := range topics {
		if strings.Contains(lowered, topic) {
			vector[i] = 1
		}
	}

	return vector
}

type topicClient struct{}

func (c *topicClient) Embeddings(_ context.Context, inputs []string, _ ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	out := make([][]float64, len(inputs))
	for i, in := range inputs {
		out[i] = topicEmbeddings(in)
	}

	return topicEmbeddingsResponse{out}, nil
}

func (c *topicClient) ChatCompletion(context.Context, ...llm.ChatCompletionOptionFunc) (llm.ChatCompletionResponse, error) {
	return nil, errors.New("not implemented")
}

func (c *topicClient) ChatCompletionStream(context.Context, ...llm.ChatCompletionOptionFunc) (<-chan llm.StreamChunk, error) {
	return nil, errors.New("not implemented")
}

type topicEmbeddingsResponse struct{ vectors [][]float64 }

func (r topicEmbeddingsResponse) Embeddings() [][]float64    { return r.vectors }
func (r topicEmbeddingsResponse) Usage() llm.EmbeddingsUsage { return nil }

// opaqueIndex forwards index.Index while hiding every optional capability of
// the index it wraps. It is what lets this test run the *same* backend down the
// fallback path: index.AsFilterable cannot see through it, so the pipeline
// declines the push-down and the Manager filters in Go instead.
type opaqueIndex struct{ wrapped index.Index }

func (o *opaqueIndex) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	return o.wrapped.Index(ctx, document, funcs...)
}
func (o *opaqueIndex) DeleteBySource(ctx context.Context, source *url.URL) error {
	return o.wrapped.DeleteBySource(ctx, source)
}
func (o *opaqueIndex) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	return o.wrapped.DeleteByID(ctx, ids...)
}
func (o *opaqueIndex) All(ctx context.Context, yield func(model.SectionID) bool) error {
	return o.wrapped.All(ctx, yield)
}
func (o *opaqueIndex) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return o.wrapped.Search(ctx, query, opts)
}

// parityDoc is one corpus document: its topic drives both its text and its
// embedding, its metadata drives the filters.
type parityDoc struct {
	source   string
	topic    string
	metadata map[string]any
}

// parityCorpus builds a corpus where each topic has many documents but only a
// few carry a given metadata value. That is the regime where the two paths can
// diverge: a filter selective enough that a top-k chosen before filtering is
// mostly eliminated.
func parityCorpus() []parityDoc {
	docs := make([]parityDoc, 0, 120)

	for i := range 120 {
		topic := topics[i%len(topics)]

		// One document in twelve is French; one in three is recent.
		lang := "en"
		if i%12 == 0 {
			lang = "fr"
		}
		year := 2019.0
		if i%3 == 0 {
			year = 2026.0
		}

		metadata := map[string]any{"lang": lang, "year": year}
		// A tenth of the corpus carries no lang at all, to exercise the
		// absent-key semantics on both paths.
		if i%10 == 7 {
			delete(metadata, "lang")
		}

		docs = append(docs, parityDoc{
			source:   fmt.Sprintf("mem://doc-%03d", i),
			topic:    topic,
			metadata: metadata,
		})
	}

	return docs
}

// TestPushdownEvalParity scores the same corpus and the same filters twice —
// once with the filter pushed into the index, once with it applied in Go — and
// requires the retrieval metrics to agree.
//
// Both runs share one backend, one ranking and one dataset; the only thing that
// differs is where the filter is evaluated. Any gap in nDCG or recall is
// therefore a semantic divergence between the two implementations, which is the
// failure mode the whole design guards against.
func TestPushdownEvalParity(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: indexes a 120-document corpus twice")
	}

	sqlitevecIndex.EnsureVecWASM()

	ctx := context.Background()
	docs := parityCorpus()

	// The corpus does not depend on the filter, so each path is indexed once
	// and every filter is evaluated against it.
	pushdownCodex := newParityCodex(t, ctx, docs, false)
	fallbackCodex := newParityCodex(t, ctx, docs, true)

	filters := []struct {
		name       string
		conditions []index.Condition
	}{
		{"unfiltered", nil},
		{"selective equality", []index.Condition{index.Eq("lang", "fr")}},
		{"absent key", []index.Condition{index.NotExists("lang")}},
		{"inequality", []index.Condition{index.Ne("lang", "fr")}},
		{"range", []index.Condition{index.Gte("year", 2020)}},
		{"conjunction", []index.Condition{index.Eq("lang", "en"), index.Gte("year", 2020)}},
	}

	for _, tc := range filters {
		t.Run(tc.name, func(t *testing.T) {
			dataset := parityDataset(docs, tc.conditions)
			if len(dataset.Queries) == 0 {
				t.Fatal("dataset is empty: the filter leaves no relevant document")
			}

			pushdown := evaluateParity(t, ctx, pushdownCodex, dataset, tc.conditions)
			fallback := evaluateParity(t, ctx, fallbackCodex, dataset, tc.conditions)

			for _, k := range []int{5, 10} {
				pushedNDCG, fallbackNDCG := pushdown.NDCGAtK[k], fallback.NDCGAtK[k]
				if pushedNDCG != fallbackNDCG {
					t.Errorf("nDCG@%d: push-down %.4f, fallback %.4f", k, pushedNDCG, fallbackNDCG)
				}

				pushedRecall, fallbackRecall := pushdown.RecallAtK[k], fallback.RecallAtK[k]
				if pushedRecall != fallbackRecall {
					t.Errorf("recall@%d: push-down %.4f, fallback %.4f", k, pushedRecall, fallbackRecall)
				}
			}

			// Per query as well: two opposite deviations would cancel out in
			// the dataset average and leave the assertions above green.
			if len(pushdown.PerQuery) != len(fallback.PerQuery) {
				t.Fatalf("query counts differ: %d vs %d", len(pushdown.PerQuery), len(fallback.PerQuery))
			}
			for i, pushedQuery := range pushdown.PerQuery {
				fallbackQuery := fallback.PerQuery[i]

				if pushedQuery.TargetRank != fallbackQuery.TargetRank {
					t.Errorf("query %q: first relevant rank %d (push-down) vs %d (fallback)",
						pushedQuery.QueryID, pushedQuery.TargetRank, fallbackQuery.TargetRank)
				}
				for _, k := range []int{5, 10} {
					if pushedQuery.NDCGAtK[k] != fallbackQuery.NDCGAtK[k] {
						t.Errorf("query %q nDCG@%d: %.4f (push-down) vs %.4f (fallback)",
							pushedQuery.QueryID, k, pushedQuery.NDCGAtK[k], fallbackQuery.NDCGAtK[k])
					}
					if pushedQuery.RecallAtK[k] != fallbackQuery.RecallAtK[k] {
						t.Errorf("query %q recall@%d: %.4f (push-down) vs %.4f (fallback)",
							pushedQuery.QueryID, k, pushedQuery.RecallAtK[k], fallbackQuery.RecallAtK[k])
					}
				}
			}

			// A filter that keeps documents must not collapse retrieval: this
			// guards against both paths agreeing on nothing at all.
			if pushdown.NDCGAtK[10] == 0 {
				t.Error("nDCG@10 is zero on both paths, the assertions above prove nothing")
			}

			t.Logf("nDCG@10=%.4f recall@10=%.4f (both paths)", pushdown.NDCGAtK[10], pushdown.RecallAtK[10])
		})
	}
}

// parityDataset builds one golden query per topic, whose relevant sources are
// the documents of that topic satisfying the filter — the ground truth the two
// paths are scored against.
func parityDataset(docs []parityDoc, conditions []index.Condition) *eval.Dataset {
	filter := index.Filter(conditions)

	dataset := &eval.Dataset{Name: "pushdown-parity"}
	for _, topic := range topics {
		query := eval.Query{ID: topic, Query: topic, Lang: "en"}

		for _, doc := range docs {
			if doc.topic != topic || !filter.Matches(doc.metadata) {
				continue
			}
			query.RelevantSources = append(query.RelevantSources, doc.source)
		}

		if len(query.RelevantSources) > 0 {
			dataset.Queries = append(dataset.Queries, query)
		}
	}

	return dataset
}

// newParityCodex indexes the corpus into a fresh Codex. When opaque is set the
// index is wrapped so its filter push-down capability is invisible, forcing the
// Go fallback over the very same backend.
func newParityCodex(t *testing.T, ctx context.Context, docs []parityDoc, opaque bool) *amoxtli.Codex {
	t.Helper()

	dir := t.TempDir()
	client := &topicClient{}

	vecDB, err := sqlite3.Open(filepath.Join(dir, "vectors.sqlite"))
	if err != nil {
		t.Fatalf("could not open sqlite-vec db: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { vecDB.Close() })

	vecIdx := sqlitevecIndex.NewIndex(vecDB, client,
		sqlitevecIndex.WithEmbeddingsModel("mock"),
		sqlitevecIndex.WithVectorSize(len(topics)),
		sqlitevecIndex.WithMaxWords(500),
	)

	var indexer index.Index = vecIdx
	if opaque {
		indexer = &opaqueIndex{wrapped: vecIdx}
	}

	store, err := gormStore.NewSQLiteStore(filepath.Join(dir, "data.sqlite"))
	if err != nil {
		t.Fatalf("could not open store: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { store.Close() })

	codex, err := amoxtli.New(ctx,
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(amoxtli.Indexer{ID: "vector", Index: indexer, Weight: 1}),
		amoxtli.WithLLMClient(client),
		amoxtli.WithDisableHyDE(),
		amoxtli.WithDisableJudge(),
	)
	if err != nil {
		t.Fatalf("could not create codex: %+v", errors.WithStack(err))
	}
	t.Cleanup(func() { codex.Close() })

	// The capability must be visible exactly when it is meant to be: without
	// this check a broken detection would silently score the same path twice.
	if _, ok := index.AsFilterable(codex.Index()); ok == opaque {
		t.Fatalf("filter push-down availability = %v, want %v (opaque=%v)", ok, !opaque, opaque)
	}

	collID, err := codex.CreateCollection(ctx, "parity")
	if err != nil {
		t.Fatalf("could not create collection: %+v", errors.WithStack(err))
	}

	for _, doc := range docs {
		source, err := url.Parse(doc.source)
		if err != nil {
			t.Fatalf("parse source %q: %+v", doc.source, errors.WithStack(err))
		}

		body := fmt.Sprintf("# %s\n\nThis note is about %s techniques.\n", doc.topic, doc.topic)

		taskID, err := codex.IndexFile(ctx, collID, filepath.Base(source.Path)+".md", strings.NewReader(body),
			amoxtli.WithIndexFileSource(source),
			amoxtli.WithIndexFileMetadata(doc.metadata),
		)
		if err != nil {
			t.Fatalf("could not index %q: %+v", doc.source, errors.WithStack(err))
		}
		waitTask(t, ctx, codex, taskID)
	}

	return codex
}

// evaluateParity scores one already-indexed Codex against the dataset, applying
// the filter through the public search API.
func evaluateParity(t *testing.T, ctx context.Context, codex *amoxtli.Codex, dataset *eval.Dataset, conditions []index.Condition) *eval.Report {
	t.Helper()

	retriever := eval.FromSearchResults(func(ctx context.Context, query string, k int) ([]*index.SearchResult, error) {
		opts := []amoxtli.SearchOption{amoxtli.WithSearchMaxResults(k)}
		if len(conditions) > 0 {
			opts = append(opts, amoxtli.WithSearchFilter(conditions...))
		}

		page, err := codex.SearchPage(ctx, query, opts...)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		return page.Results, nil
	})

	report, err := eval.Evaluate(ctx, dataset, retriever, 5, 10)
	if err != nil {
		t.Fatalf("evaluate: %+v", errors.WithStack(err))
	}

	return report
}
