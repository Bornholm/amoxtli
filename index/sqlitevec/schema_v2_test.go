package sqlitevec

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/markdown"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"

	sqlite_vec "github.com/bornholm/amoxtli/index/sqlitevec/internal/vec"
)

// indexDocInCollection indexes a one-section document attached to the given
// collections.
func indexDocInCollection(t *testing.T, ctx context.Context, idx *Index, source, body string, collections ...model.CollectionID) {
	t.Helper()
	doc, err := markdown.Parse([]byte(body))
	if err != nil {
		t.Fatalf("could not parse document: %+v", err)
	}
	u, err := url.Parse(source)
	if err != nil {
		t.Fatalf("could not parse source: %+v", err)
	}
	doc.SetSource(u)
	for _, coll := range collections {
		doc.AddCollection(model.NewCollection(coll, string(coll), ""))
	}
	if err := idx.Index(ctx, doc); err != nil {
		t.Fatalf("could not index %s: %+v", source, errors.WithStack(err))
	}
}

// TestFilteredSearchReturnsKResults pins the recall fix of the partitioned
// schema: with the old post-KNN JOIN filter, the global top-k could live
// entirely in another collection and the filtered search silently returned
// nothing. With partition pruning, the filtered collection must yield its own
// k nearest chunks regardless of the other collections' content.
func TestFilteredSearchReturnsKResults(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	collA := model.CollectionID("coll-a")
	collB := model.CollectionID("coll-b")

	// Collection B holds 5 chunks nearly identical to the query (ALPHA);
	// collection A holds 3 chunks far from it (GAMMA). The global top-3 all
	// live in B.
	for n := range 5 {
		indexDocInCollection(t, ctx, idx, fmt.Sprintf("mem://b-%d", n), "# B\n\nThis is about ALPHA topics.\n", collB)
	}
	for n := range 3 {
		indexDocInCollection(t, ctx, idx, fmt.Sprintf("mem://a-%d", n), "# A\n\nThis is about GAMMA topics.\n", collA)
	}

	results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{
		MaxResults:  3,
		Collections: []model.CollectionID{collA},
	})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}

	total := 0
	for _, r := range results {
		if got := r.Source.String(); got[:len("mem://a-")] != "mem://a-" {
			t.Errorf("filtered search returned a result outside the collection: %s", got)
		}
		total += len(r.Sections)
	}
	if total != 3 {
		t.Errorf("filtered search returned %d sections, want 3 (the collection holds 3 chunks)", total)
	}
}

// TestUnfilteredSearchDeduplicatesMultiCollectionChunks checks that a chunk
// belonging to several collections (one vec0 row per collection) appears only
// once in an unfiltered search.
func TestUnfilteredSearchDeduplicatesMultiCollectionChunks(t *testing.T) {
	ctx := context.Background()
	idx := newHermeticIndex(t, &keywordClient{})

	indexDocInCollection(t, ctx, idx, "mem://multi", "# Multi\n\nThis is about ALPHA topics.\n",
		model.CollectionID("coll-a"), model.CollectionID("coll-b"))

	results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if got := len(results[0].Sections); got != 1 {
		t.Errorf("expected the multi-collection chunk once, got %d sections", got)
	}
}

// TestCoarseQuantizationSearch checks the two-stage (Hamming + float
// re-scoring) search returns the same top result as the direct scan.
func TestCoarseQuantizationSearch(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")

	// Vector size 8 (divisible by 8) so the coarse column exists; the keyword
	// client emits 4-dim vectors, padded below via a wrapping client is not
	// needed: vec_slice(?, 0, 8) requires >= 8 dims, so use a client variant.
	idx, err := NewIndexAtPath(dbFile, &paddedKeywordClient{},
		WithEmbeddingsModel("mock"),
		WithVectorSize(8),
		WithReadPoolSize(1),
		WithCoarseQuantization(true),
	)
	if err != nil {
		t.Fatalf("could not open index: %+v", errors.WithStack(err))
	}
	defer idx.Close()

	indexDocInCollection(t, ctx, idx, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n", "coll-a")
	indexDocInCollection(t, ctx, idx, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n", "coll-a")

	// Filtered and unfiltered, both through the coarse path.
	for _, opts := range []index.SearchOptions{
		{MaxResults: 5},
		{MaxResults: 5, Collections: []model.CollectionID{"coll-a"}},
	} {
		results, err := idx.Search(ctx, "a question about ALPHA", opts)
		if err != nil {
			t.Fatalf("coarse search failed: %+v", errors.WithStack(err))
		}
		if len(results) == 0 {
			t.Fatal("coarse search returned no results")
		}
		if got := results[0].Source.String(); got != "mem://alpha" {
			t.Errorf("results[0].Source = %s, want mem://alpha", got)
		}
	}
}

// paddedKeywordClient is the keywordClient with vectors zero-padded to 8
// dimensions (binary quantization requires a multiple of 8).
type paddedKeywordClient struct {
	keywordClient
}

func (c *paddedKeywordClient) Embeddings(ctx context.Context, inputs []string, funcs ...llm.EmbeddingsOptionFunc) (llm.EmbeddingsResponse, error) {
	res, err := c.keywordClient.Embeddings(ctx, inputs, funcs...)
	if err != nil {
		return nil, err
	}
	padded := make([][]float64, 0, len(inputs))
	for _, v := range res.Embeddings() {
		p := make([]float64, 8)
		copy(p, v)
		padded = append(padded, p)
	}
	return keywordEmbeddingsResponse{padded}, nil
}

// TestSnapshotRestoreRoundtrip checks Backup/Restore of the vector index: the
// restored index must serve the same searches, including collection-filtered
// ones (this path was broken before the v2 schema: the snapshot SQL read a
// column that no longer existed).
func TestSnapshotRestoreRoundtrip(t *testing.T) {
	ctx := context.Background()
	client := &keywordClient{}
	source := newHermeticIndex(t, client)

	indexDocInCollection(t, ctx, source, "mem://alpha", "# Alpha\n\nThis section is about ALPHA topics.\n", "coll-a")
	indexDocInCollection(t, ctx, source, "mem://beta", "# Beta\n\nThis section is about BETA topics.\n", "coll-b")

	snapshot, err := source.GenerateSnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot failed: %+v", errors.WithStack(err))
	}
	data, err := io.ReadAll(snapshot)
	if err != nil {
		t.Fatalf("could not read snapshot: %+v", errors.WithStack(err))
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("could not close snapshot: %+v", errors.WithStack(err))
	}

	restored := newHermeticIndex(t, client)
	if err := restored.RestoreSnapshot(ctx, bytes.NewReader(data)); err != nil {
		t.Fatalf("restore failed: %+v", errors.WithStack(err))
	}

	results, err := restored.Search(ctx, "a question about ALPHA", index.SearchOptions{
		MaxResults:  5,
		Collections: []model.CollectionID{"coll-a"},
	})
	if err != nil {
		t.Fatalf("search on restored index failed: %+v", errors.WithStack(err))
	}
	if len(results) != 1 || results[0].Source.String() != "mem://alpha" {
		t.Fatalf("unexpected results on restored index: %+v", results)
	}
}

// TestV1SchemaMigration builds a v1 database by hand (single unpartitioned
// embeddings_vec table) and checks that opening it upgrades to the
// partitioned schema, preserving vectors and collections without recomputing
// embeddings.
func TestV1SchemaMigration(t *testing.T) {
	ctx := context.Background()
	dbFile := filepath.Join(t.TempDir(), "index.sqlite")

	conn, err := sqlite3.Open(dbFile)
	if err != nil {
		t.Fatalf("open: %+v", err)
	}

	v1Schema := []string{
		`CREATE TABLE embeddings ( id INTEGER NOT NULL PRIMARY KEY, source TEXT, section_id TEXT, chunk_index INTEGER DEFAULT 0 );`,
		`CREATE TABLE embeddings_collections ( embeddings_id INTEGER, collection_id TEXT NOT NULL );`,
		`CREATE TABLE amoxtli_meta ( key TEXT PRIMARY KEY, value TEXT NOT NULL );`,
		`INSERT INTO amoxtli_meta (key, value) VALUES ('embeddings_model', 'mock'), ('vector_size', '4');`,
		`CREATE VIRTUAL TABLE embeddings_vec USING vec0(embedding float[4]);`,
	}
	for _, sql := range v1Schema {
		if err := conn.Exec(sql); err != nil {
			t.Fatalf("v1 schema: %v (%s)", err, sql)
		}
	}

	// Two chunks: one in coll-a, one without any collection.
	insertV1 := func(id int, source, sectionID string, vec []float32, collection string) {
		t.Helper()
		if err := conn.Exec(fmt.Sprintf(
			`INSERT INTO embeddings (id, source, section_id) VALUES (%d, '%s', '%s');`, id, source, sectionID)); err != nil {
			t.Fatal(err)
		}
		blob, err := sqlite_vec.SerializeFloat32(vec)
		if err != nil {
			t.Fatal(err)
		}
		stmt, _, err := conn.Prepare("INSERT INTO embeddings_vec (rowid, embedding) VALUES (?, ?);")
		if err != nil {
			t.Fatal(err)
		}
		if err := stmt.BindInt(1, id); err != nil {
			t.Fatal(err)
		}
		if err := stmt.BindBlob(2, blob); err != nil {
			t.Fatal(err)
		}
		if err := stmt.Exec(); err != nil {
			t.Fatal(err)
		}
		stmt.Close()
		if collection != "" {
			if err := conn.Exec(fmt.Sprintf(
				`INSERT INTO embeddings_collections (embeddings_id, collection_id) VALUES (%d, '%s');`, id, collection)); err != nil {
				t.Fatal(err)
			}
		}
	}

	insertV1(1, "mem://alpha", "sec-alpha", []float32{1, 0.01, 0.01, 0.01}, "coll-a")
	insertV1(2, "mem://beta", "sec-beta", []float32{0.01, 1, 0.01, 0.01}, "")

	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening the index migrates to v2 eagerly.
	idx, err := NewIndexAtPath(dbFile, &keywordClient{},
		WithEmbeddingsModel("mock"),
		WithVectorSize(4),
		WithReadPoolSize(1),
	)
	if err != nil {
		t.Fatalf("could not open migrated index: %+v", errors.WithStack(err))
	}
	defer idx.Close()

	// Filtered search must find the coll-a chunk (vector preserved).
	results, err := idx.Search(ctx, "a question about ALPHA", index.SearchOptions{
		MaxResults:  5,
		Collections: []model.CollectionID{"coll-a"},
	})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}
	if len(results) != 1 || results[0].Source.String() != "mem://alpha" {
		t.Fatalf("expected the migrated coll-a chunk, got %+v", results)
	}

	// Unfiltered search must see both chunks, including the collection-less one.
	results, err = idx.Search(ctx, "a question about BETA", index.SearchOptions{MaxResults: 5})
	if err != nil {
		t.Fatalf("search failed: %+v", errors.WithStack(err))
	}
	if len(results) == 0 || results[0].Source.String() != "mem://beta" {
		t.Fatalf("expected the migrated collection-less chunk first, got %+v", results)
	}
}
