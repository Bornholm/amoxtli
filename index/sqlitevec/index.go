package sqlitevec

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"

	sqlite_vec "github.com/bornholm/amoxtli/index/sqlitevec/internal/vec"
)

type Index struct {
	maxWords              int
	vectorSize            int
	embeddingsConcurrency int
	// coarse enables the two-stage binary-quantization search (only honored
	// when the vector size supports it, i.e. divisible by 8).
	coarse  bool
	getConn func(ctx context.Context) (*sqlite3.Conn, error)
	llm     llm.Client
	model   string
	// rwLock allows concurrent Search operations while serializing Index/Delete
	rwLock sync.RWMutex
	// readers is the pool of dedicated read connections (NewIndexAtPath). When
	// nil (NewIndex), searches share the single caller-owned connection under
	// the read lock; with a pool, searches run on their own connection and rely
	// on WAL snapshot isolation instead, so they neither serialize on the
	// writer nor on each other.
	readers chan *sqlite3.Conn
	// ownedConns are the connections opened (and closed) by the index itself.
	ownedConns []*sqlite3.Conn
}

// DefaultMaxWords bounds, by default, the number of words per chunk sent to the
// embeddings model.
const DefaultMaxWords int = 500

// DefaultEmbeddingsConcurrency bounds, by default, how many embedding batches
// are computed in parallel for a single document. 8 gives most of the speedup
// on large documents while staying modest enough to avoid overwhelming the
// embeddings endpoint (rate limiting / 429s); lower it via config for stricter
// endpoints.
const DefaultEmbeddingsConcurrency int = 8

// DefaultReadPoolSize is the default number of read connections opened by
// NewIndexAtPath for concurrent searches.
const DefaultReadPoolSize int = 4

// Options configures a sqlite-vec Index.
type Options struct {
	// EmbeddingsModel identifies the embeddings model. It is recorded in the
	// index and opening an existing index with a different model is rejected
	// (the stored vectors would no longer be comparable).
	EmbeddingsModel string
	// VectorSize is the dimension of the vec0 embedding column (default
	// DefaultVectorSize). It is recorded in the index; opening an existing
	// index with a different size is rejected.
	VectorSize int
	// MaxWords bounds the size of the chunks sent to the embeddings model.
	MaxWords int
	// EmbeddingsConcurrency bounds how many embedding batches are computed in
	// parallel for a single document. Large documents split into many batches;
	// computing them concurrently cuts indexing latency at the cost of more
	// simultaneous requests to the embeddings endpoint. Defaults to
	// DefaultEmbeddingsConcurrency when <= 0.
	EmbeddingsConcurrency int
	// ReadPoolSize is the number of dedicated read connections opened by
	// NewIndexAtPath (defaults to DefaultReadPoolSize when <= 0). Ignored by
	// NewIndex, which uses the single caller-owned connection.
	ReadPoolSize int
	// CoarseQuantization enables the two-stage search: a fast KNN on the
	// binary-quantized vectors (Hamming distance) preselects k×8 candidates,
	// re-scored with the full float vectors. ~30× faster scans at a marginal
	// quality cost, worthwhile on large corpora. Requires a vector size
	// divisible by 8.
	CoarseQuantization bool
}

type OptionFunc func(opts *Options)

// WithEmbeddingsModel records the embeddings model backing the index.
func WithEmbeddingsModel(model string) OptionFunc {
	return func(opts *Options) {
		opts.EmbeddingsModel = model
	}
}

// WithVectorSize sets the dimension of the vec0 embedding column.
func WithVectorSize(size int) OptionFunc {
	return func(opts *Options) {
		opts.VectorSize = size
	}
}

// WithMaxWords bounds the size of the chunks sent to the embeddings model.
func WithMaxWords(maxWords int) OptionFunc {
	return func(opts *Options) {
		opts.MaxWords = maxWords
	}
}

// WithEmbeddingsConcurrency bounds how many embedding batches are computed in
// parallel for a single document (see Options.EmbeddingsConcurrency).
func WithEmbeddingsConcurrency(n int) OptionFunc {
	return func(opts *Options) {
		opts.EmbeddingsConcurrency = n
	}
}

// WithReadPoolSize sets the number of dedicated read connections opened by
// NewIndexAtPath (see Options.ReadPoolSize).
func WithReadPoolSize(n int) OptionFunc {
	return func(opts *Options) {
		opts.ReadPoolSize = n
	}
}

// WithCoarseQuantization toggles the two-stage binary-quantization search
// (see Options.CoarseQuantization).
func WithCoarseQuantization(enabled bool) OptionFunc {
	return func(opts *Options) {
		opts.CoarseQuantization = enabled
	}
}

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		VectorSize:            DefaultVectorSize,
		MaxWords:              DefaultMaxWords,
		EmbeddingsConcurrency: DefaultEmbeddingsConcurrency,
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// DeleteByID implements index.Index.
func (i *Index) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	i.rwLock.Lock()
	defer i.rwLock.Unlock()

	err := i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		// First, get the embeddings IDs to delete
		getIDsStmt, _, err := conn.Prepare("SELECT id FROM embeddings WHERE section_id IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer getIDsStmt.Close()

		jsonIDs, err := json.Marshal(ids)
		if err != nil {
			return errors.WithStack(err)
		}

		if err := getIDsStmt.BindBlob(1, jsonIDs); err != nil {
			return errors.WithStack(err)
		}

		var idsToDelete []int
		for getIDsStmt.Step() {
			idsToDelete = append(idsToDelete, getIDsStmt.ColumnInt(0))
		}

		if len(idsToDelete) == 0 {
			return nil
		}

		if err := deleteVecRows(conn, idsToDelete); err != nil {
			return errors.WithStack(err)
		}

		// Delete from embeddings
		delStmt, _, err := conn.Prepare("DELETE FROM embeddings WHERE section_id IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer delStmt.Close()

		if err := delStmt.BindBlob(1, jsonIDs); err != nil {
			return errors.WithStack(err)
		}

		if err := delStmt.Exec(); err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// deleteVecRows removes every vector-side row (embeddings_collections,
// embeddings_vec2, embeddings_vec_map) attached to the given embeddings ids.
// Callers hold the write lock and run inside a retry/savepoint.
func deleteVecRows(conn *sqlite3.Conn, embeddingsIDs []int) error {
	jsonIDsBytes, err := json.Marshal(embeddingsIDs)
	if err != nil {
		return errors.WithStack(err)
	}
	jsonIDs := string(jsonIDsBytes)

	statements := []string{
		"DELETE FROM embeddings_collections WHERE embeddings_id IN ( SELECT value FROM json_each(?) );",
		// The vec0 rowids are the map ids of the deleted chunks.
		"DELETE FROM embeddings_vec2 WHERE rowid IN ( SELECT id FROM embeddings_vec_map WHERE embeddings_id IN ( SELECT value FROM json_each(?) ) );",
		"DELETE FROM embeddings_vec_map WHERE embeddings_id IN ( SELECT value FROM json_each(?) );",
	}

	for _, sql := range statements {
		stmt, _, err := conn.Prepare(sql)
		if err != nil {
			return errors.WithStack(err)
		}

		if err := stmt.BindText(1, jsonIDs); err != nil {
			stmt.Close()
			return errors.WithStack(err)
		}

		if err := stmt.Exec(); err != nil {
			stmt.Close()
			return errors.WithStack(err)
		}

		if err := stmt.Close(); err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

// deleteBySourceLocked performs the deletion for a given source without acquiring the lock.
// Callers must hold i.rwLock before calling this method.
func (i *Index) deleteBySourceLocked(ctx context.Context, source *url.URL) error {
	return i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		// First, get the embeddings IDs to delete
		getIDsStmt, _, err := conn.Prepare("SELECT id FROM embeddings WHERE source = ?;")
		if err != nil {
			return errors.WithStack(err)
		}
		defer getIDsStmt.Close()

		if err := getIDsStmt.BindText(1, source.String()); err != nil {
			return errors.WithStack(err)
		}

		var idsToDelete []int
		for getIDsStmt.Step() {
			idsToDelete = append(idsToDelete, getIDsStmt.ColumnInt(0))
		}

		if len(idsToDelete) == 0 {
			return nil
		}

		if err := deleteVecRows(conn, idsToDelete); err != nil {
			return errors.WithStack(err)
		}

		// Delete from embeddings
		delStmt, _, err := conn.Prepare("DELETE FROM embeddings WHERE source = ?;")
		if err != nil {
			return errors.WithStack(err)
		}
		defer delStmt.Close()

		if err := delStmt.BindText(1, source.String()); err != nil {
			return errors.WithStack(err)
		}

		if err := delStmt.Exec(); err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
}

// DeleteBySource implements index.Index.
func (i *Index) DeleteBySource(ctx context.Context, source *url.URL) error {
	i.rwLock.Lock()
	defer i.rwLock.Unlock()

	return errors.WithStack(i.deleteBySourceLocked(ctx, source))
}

type indexableChunk struct {
	Section  model.Section
	Text     string
	ChunkIdx int
}

func estimateTokens(text string) int {
	return len(text) / charsPerToken
}

const (
	// charsPerToken is the chars-per-token ratio used to budget embedding
	// batches. 3 is deliberately conservative: French prose sits around 3
	// chars/token (English closer to 4), and underestimating tokens would
	// overflow targetBatchTokens.
	charsPerToken = 3

	maxBatchItemCount = 100

	targetBatchTokens = 6000

	overlapChars = 200
)

// Index implements index.Index.
func (i *Index) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	opts := index.NewOptions(funcs...)

	source := document.Source()

	chunksToProcess, err := i.collectChunks(document)
	if err != nil {
		return errors.WithStack(err)
	}

	defer func() {
		if opts.OnProgress != nil {
			opts.OnProgress(1.0)
		}
	}()

	// Compute the embeddings BEFORE taking the write lock: the LLM calls are the
	// slow part, and holding the write lock across them would block every
	// concurrent Search (which only needs the read lock) for their whole
	// duration. As a bonus, a failure here leaves the existing content intact
	// (the delete below only runs once the vectors are ready).
	vectors, err := i.computeEmbeddings(ctx, chunksToProcess)
	if err != nil {
		return errors.WithStack(err)
	}

	i.rwLock.Lock()
	defer i.rwLock.Unlock()

	if source != nil {
		if err := i.deleteBySourceLocked(ctx, source); err != nil {
			return errors.WithStack(err)
		}
	}

	if len(chunksToProcess) == 0 {
		return nil
	}

	return i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		// Insert into main embeddings table (metadata only - vectors go to vec0)
		stmt, _, err := conn.Prepare(`
			INSERT INTO embeddings (source, section_id, chunk_index)
			VALUES (?, ?, ?)
			RETURNING id;
		`)
		if err != nil {
			return errors.WithStack(err)
		}
		defer stmt.Close()

		mapStmt, _, err := conn.Prepare(`
			INSERT INTO embeddings_vec_map (embeddings_id, collection_id)
			VALUES (?, ?)
			RETURNING id;
		`)
		if err != nil {
			return errors.WithStack(err)
		}
		defer mapStmt.Close()

		// One vec0 row per (chunk, collection): the collection is the vec0
		// partition key so filtered KNN queries scan only that collection.
		vecStmt, _, err := conn.Prepare(i.insertVecSQL())
		if err != nil {
			return errors.WithStack(err)
		}
		defer vecStmt.Close()

		for idx, item := range chunksToProcess {
			vecBlob := vectors[idx]

			if err := stmt.BindText(1, item.Section.Document().Source().String()); err != nil {
				return err
			}
			if err := stmt.BindText(2, string(item.Section.ID())); err != nil {
				return err
			}
			if err := stmt.BindInt64(3, int64(item.ChunkIdx)); err != nil {
				return err
			}

			if hasRow := stmt.Step(); !hasRow {
				return errors.New("no id returned")
			}

			embeddingsID := stmt.ColumnInt(0)
			stmt.Reset()

			// Chunks without any collection land in the '' partition so
			// unfiltered searches still see them.
			partitions := []string{""}
			collections := item.Section.Document().Collections()
			if len(collections) > 0 {
				partitions = partitions[:0]
				for _, coll := range collections {
					partitions = append(partitions, string(coll.ID()))
				}
			}

			for _, partition := range partitions {
				if err := mapStmt.BindInt(1, embeddingsID); err != nil {
					return err
				}
				if err := mapStmt.BindText(2, partition); err != nil {
					return err
				}
				if hasRow := mapStmt.Step(); !hasRow {
					return errors.New("no vec map id returned")
				}
				vecRowID := mapStmt.ColumnInt64(0)
				mapStmt.Reset()

				if err := bindVecInsert(vecStmt, vecRowID, vecBlob, partition, i.hasCoarse()); err != nil {
					return errors.WithStack(err)
				}
				if err := vecStmt.Exec(); err != nil {
					return err
				}
				vecStmt.Reset()
			}

			for _, coll := range item.Section.Document().Collections() {
				if err := i.insertCollection(ctx, conn, embeddingsID, coll.ID()); err != nil {
					return errors.WithStack(err)
				}
			}
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
}

// hasCoarse reports whether the index stores (and may search) the
// binary-quantized column — the vector size must be divisible by 8.
func (i *Index) hasCoarse() bool {
	return supportsCoarse(i.vectorSize)
}

// insertVecSQL builds the vec0 insert statement, including the quantized
// column when the schema has one.
func (i *Index) insertVecSQL() string {
	if i.hasCoarse() {
		return fmt.Sprintf(`
			INSERT INTO embeddings_vec2 (rowid, embedding, embedding_coarse, collection_id)
			VALUES (?, vec_normalize(vec_slice(?, 0, %d)), vec_quantize_binary(vec_normalize(vec_slice(?, 0, %d))), ?);
		`, i.vectorSize, i.vectorSize)
	}
	return fmt.Sprintf(`
		INSERT INTO embeddings_vec2 (rowid, embedding, collection_id)
		VALUES (?, vec_normalize(vec_slice(?, 0, %d)), ?);
	`, i.vectorSize)
}

// bindVecInsert binds the parameters of an insertVecSQL statement.
func bindVecInsert(stmt *sqlite3.Stmt, rowID int64, vecBlob []byte, partition string, coarse bool) error {
	if err := stmt.BindInt64(1, rowID); err != nil {
		return errors.WithStack(err)
	}
	if err := stmt.BindBlob(2, vecBlob); err != nil {
		return errors.WithStack(err)
	}
	next := 3
	if coarse {
		if err := stmt.BindBlob(3, vecBlob); err != nil {
			return errors.WithStack(err)
		}
		next = 4
	}
	if err := stmt.BindText(next, partition); err != nil {
		return errors.WithStack(err)
	}
	return nil
}

// collectChunks walks the document sections and splits their content into the
// chunks to embed (character-window with overlap for oversized sections). It
// performs no I/O beyond reading section content and takes no lock.
func (i *Index) collectChunks(document model.Document) ([]*indexableChunk, error) {
	var chunksToProcess []*indexableChunk

	limitChars := i.maxWords * 6

	var collect func(s model.Section) error
	collect = func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}
		// Trim surrounding whitespace: it carries no meaning for the embedding
		// and it keeps the chunk text — and thus any embeddings-cache key —
		// stable between a fresh parse and the store round-trip (the persisted
		// content is trimmed, the parsed one may keep a trailing newline).
		textStr := strings.TrimSpace(string(content))
		textLen := len(textStr)

		if textLen <= limitChars {
			if textLen > 0 { // On ignore les sections vides
				chunksToProcess = append(chunksToProcess, &indexableChunk{
					Section:  s,
					Text:     textStr,
					ChunkIdx: 0,
				})
			}
		} else {
			runes := []rune(textStr)
			runesLen := len(runes)

			limitRunes := limitChars
			overlapRunes := overlapChars

			currentChunkIdx := 0
			for start := 0; start < runesLen; {
				end := start + limitRunes
				if end > runesLen {
					end = runesLen
				}

				chunkText := string(runes[start:end])

				chunksToProcess = append(chunksToProcess, &indexableChunk{
					Section:  s,
					Text:     chunkText,
					ChunkIdx: currentChunkIdx,
				})
				currentChunkIdx++

				if end == runesLen {
					break
				}

				start += (limitRunes - overlapRunes)
			}
		}

		for _, child := range s.Sections() {
			if err := collect(child); err != nil {
				return errors.WithStack(err)
			}
		}
		return nil
	}

	for _, s := range document.Sections() {
		if err := collect(s); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	return chunksToProcess, nil
}

// computeEmbeddings runs the embeddings model over every chunk (same batching
// as before) and returns the serialized vectors aligned by index with chunks.
// It performs no database access and holds no lock, so it runs concurrently
// with searches.
func (i *Index) computeEmbeddings(ctx context.Context, chunks []*indexableChunk) ([][]byte, error) {
	vectors := make([][]byte, len(chunks))

	// First, partition the chunks into batches bounded by item count and token
	// budget (same policy as before, just materialized up front).
	type embeddingsBatch struct {
		idx   []int
		texts []string
	}

	var (
		batches     []embeddingsBatch
		batchIdx    []int
		batchTexts  []string
		batchTokens int
	)

	seal := func() {
		if len(batchTexts) == 0 {
			return
		}
		batches = append(batches, embeddingsBatch{idx: batchIdx, texts: batchTexts})
		batchIdx, batchTexts, batchTokens = nil, nil, 0
	}

	for idx, chunk := range chunks {
		tokenEst := estimateTokens(chunk.Text)

		isBatchFull := (len(batchTexts) >= maxBatchItemCount) ||
			(batchTokens+tokenEst >= targetBatchTokens)

		if isBatchFull {
			seal()
		}

		batchIdx = append(batchIdx, idx)
		batchTexts = append(batchTexts, chunk.Text)
		batchTokens += tokenEst
	}
	seal()

	if len(batches) == 0 {
		return vectors, nil
	}

	// Compute the batches concurrently: the embedding round-trips dominate
	// indexing time for large documents (many batches), and parallelizing across
	// files does not help a single big file — it is one task. Each batch writes
	// to disjoint indices of vectors, so no synchronization is required on the
	// result slice. errgroup cancels the remaining batches on the first error.
	concurrency := i.embeddingsConcurrency
	if concurrency <= 0 {
		concurrency = DefaultEmbeddingsConcurrency
	}
	concurrency = min(concurrency, len(batches))

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, b := range batches {
		g.Go(func() error {
			res, err := i.llm.Embeddings(ctx, b.texts)
			if err != nil {
				return errors.Wrap(err, "generation failed")
			}

			embeddings := res.Embeddings()
			if len(embeddings) != len(b.texts) {
				return errors.New("vector count mismatch")
			}

			for j, chunkIdx := range b.idx {
				blob, err := sqlite_vec.SerializeFloat32(toFloat32(embeddings[j]))
				if err != nil {
					return errors.WithStack(err)
				}
				vectors[chunkIdx] = blob
			}

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, errors.WithStack(err)
	}

	return vectors, nil
}

func (i *Index) insertCollection(ctx context.Context, conn *sqlite3.Conn, embeddingsID int, collectionID model.CollectionID) error {
	deleteStmt, _, err := conn.Prepare("DELETE FROM embeddings_collections WHERE embeddings_id = ? and collection_id = ?;")
	if err != nil {
		return errors.WithStack(err)
	}

	defer deleteStmt.Close()

	if err := deleteStmt.BindInt(1, embeddingsID); err != nil {
		return errors.WithStack(err)
	}

	if err := deleteStmt.BindText(2, string(collectionID)); err != nil {
		return errors.WithStack(err)
	}

	if err := deleteStmt.Exec(); err != nil {
		return errors.WithStack(err)
	}

	deleteStmt.Close()

	insertStmt, _, err := conn.Prepare("INSERT INTO embeddings_collections ( embeddings_id, collection_id ) VALUES (?, ?);")
	if err != nil {
		return errors.WithStack(err)
	}

	defer insertStmt.Close()

	if err := insertStmt.BindInt(1, embeddingsID); err != nil {
		return errors.WithStack(err)
	}

	if err := insertStmt.BindText(2, string(collectionID)); err != nil {
		return errors.WithStack(err)
	}

	if err := insertStmt.Exec(); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// Search implements index.Index.
func (i *Index) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	// Embed the query BEFORE taking the read lock and outside the retry loop:
	// a SQLITE_BUSY/LOCKED retry must repeat only the SQL, never the billable
	// network round-trip, and the call must not extend the lock's critical
	// section.
	res, err := i.llm.Embeddings(ctx, []string{query})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	vectors := res.Embeddings()
	if len(vectors) == 0 {
		return nil, errors.New("embeddings response is empty")
	}

	queryVec, err := sqlite_vec.SerializeFloat32(toFloat32(vectors[0]))
	if err != nil {
		return nil, errors.WithStack(err)
	}

	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 10 // default
	}

	conn, release, err := i.acquireReader(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer release()

	var searchResults []*index.SearchResult
	err = retryOnConn(ctx, conn, func(ctx context.Context, conn *sqlite3.Conn) error {
		// One KNN leg per filtered collection: the collection is the vec0
		// partition key, so each leg scans only that collection's rows and is
		// guaranteed to return up to maxResults matches from it (the previous
		// post-KNN JOIN filter could silently return fewer than k results
		// when the global top-k lived in other collections). Without a filter
		// a single leg scans every partition, oversampled to absorb the
		// duplicate rows of multi-collection chunks.
		var matches []vecMatch

		if len(opts.Collections) > 0 {
			for _, collection := range opts.Collections {
				legMatches, err := i.knnLeg(conn, queryVec, maxResults, string(collection))
				if err != nil {
					return errors.WithStack(err)
				}
				matches = append(matches, legMatches...)
			}
		} else {
			matches, err = i.knnLeg(conn, queryVec, maxResults*unfilteredOversample, allPartitions)
			if err != nil {
				return errors.WithStack(err)
			}
		}

		searchResults, err = resolveMatches(conn, matches, maxResults)
		if err != nil {
			return errors.WithStack(err)
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return searchResults, nil
}

// vecMatch is one KNN hit: the embeddings_vec2 rowid (= embeddings_vec_map
// id) and its distance to the query vector.
type vecMatch struct {
	rowID    int64
	distance float64
}

// allPartitions makes knnLeg scan every partition (no collection filter).
const allPartitions = ""

// unfilteredOversample widens the KNN of an unfiltered search: a chunk
// belonging to several collections has one vec0 row per collection, so the
// raw top-k may contain duplicates that deduplication then removes.
const unfilteredOversample = 4

// coarseOversample is the candidate multiplier of the two-stage search: the
// binary (Hamming) stage preselects k×coarseOversample rows, re-scored with
// the float vectors. ×8 keeps the quality loss marginal (<1% in the
// sqlite-vec literature).
const coarseOversample = 8

// knnLeg runs one KNN query, restricted to a collection partition unless
// partition is allPartitions. Note vec0 performs an exhaustive scan (there is
// no ANN index); partitioning and coarse quantization are the two levers that
// bound its cost.
func (i *Index) knnLeg(conn *sqlite3.Conn, queryVec []byte, k int, partition string) ([]vecMatch, error) {
	if i.coarse && i.hasCoarse() {
		return i.knnCoarse(conn, queryVec, k, partition)
	}
	return i.knnDirect(conn, queryVec, k, partition)
}

// knnDirect is the single-stage KNN on the float vectors.
func (i *Index) knnDirect(conn *sqlite3.Conn, queryVec []byte, k int, partition string) ([]vecMatch, error) {
	sql := fmt.Sprintf(`
		SELECT v.rowid, v.distance
		FROM embeddings_vec2 v
		WHERE v.embedding MATCH vec_normalize(vec_slice(?, 0, %d))
		AND k = ?`, i.vectorSize)
	if partition != allPartitions {
		sql += ` AND v.collection_id = ?`
	}
	sql += ` ORDER BY v.distance ASC;`

	stmt, _, err := conn.Prepare(sql)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindBlob(1, queryVec); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := stmt.BindInt(2, k); err != nil {
		return nil, errors.WithStack(err)
	}
	if partition != allPartitions {
		if err := stmt.BindText(3, partition); err != nil {
			return nil, errors.WithStack(err)
		}
	}

	var matches []vecMatch
	for stmt.Step() {
		matches = append(matches, vecMatch{rowID: stmt.ColumnInt64(0), distance: stmt.ColumnFloat(1)})
	}
	if err := stmt.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	return matches, nil
}

// knnCoarse is the two-stage KNN: Hamming preselection on the binary-quantized
// column, then float re-scoring of the candidates.
func (i *Index) knnCoarse(conn *sqlite3.Conn, queryVec []byte, k int, partition string) ([]vecMatch, error) {
	sql := fmt.Sprintf(`
		SELECT v.rowid
		FROM embeddings_vec2 v
		WHERE v.embedding_coarse MATCH vec_quantize_binary(vec_normalize(vec_slice(?, 0, %d)))
		AND k = ?`, i.vectorSize)
	if partition != allPartitions {
		sql += ` AND v.collection_id = ?`
	}
	sql += ` ORDER BY distance ASC;`

	stmt, _, err := conn.Prepare(sql)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	if err := stmt.BindBlob(1, queryVec); err != nil {
		stmt.Close()
		return nil, errors.WithStack(err)
	}
	if err := stmt.BindInt(2, k*coarseOversample); err != nil {
		stmt.Close()
		return nil, errors.WithStack(err)
	}
	if partition != allPartitions {
		if err := stmt.BindText(3, partition); err != nil {
			stmt.Close()
			return nil, errors.WithStack(err)
		}
	}

	var candidates []int64
	for stmt.Step() {
		candidates = append(candidates, stmt.ColumnInt64(0))
	}
	if err := stmt.Err(); err != nil {
		stmt.Close()
		return nil, errors.WithStack(err)
	}
	if err := stmt.Close(); err != nil {
		return nil, errors.WithStack(err)
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	// Re-score the candidates with the float vectors (same L2-on-normalized
	// metric as the direct KNN), then keep the top k.
	jsonCandidates, err := json.Marshal(candidates)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	rescoreSQL := fmt.Sprintf(`
		SELECT v.rowid, vec_distance_l2(v.embedding, vec_normalize(vec_slice(?, 0, %d)))
		FROM embeddings_vec2 v
		WHERE v.rowid IN ( SELECT value FROM json_each(?) );`, i.vectorSize)

	rescore, _, err := conn.Prepare(rescoreSQL)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer rescore.Close()

	if err := rescore.BindBlob(1, queryVec); err != nil {
		return nil, errors.WithStack(err)
	}
	if err := rescore.BindText(2, string(jsonCandidates)); err != nil {
		return nil, errors.WithStack(err)
	}

	var matches []vecMatch
	for rescore.Step() {
		matches = append(matches, vecMatch{rowID: rescore.ColumnInt64(0), distance: rescore.ColumnFloat(1)})
	}
	if err := rescore.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	slices.SortFunc(matches, func(a, b vecMatch) int {
		return cmp.Compare(a.distance, b.distance)
	})
	if len(matches) > k {
		matches = matches[:k]
	}

	return matches, nil
}

// resolveMatches maps the KNN hits back to chunks (via embeddings_vec_map),
// deduplicates chunks reached through several collection partitions (keeping
// their best distance), truncates to the maxResults best chunks and
// aggregates them into per-source results, scored like the previous
// implementation (sum of 1/distance).
func resolveMatches(conn *sqlite3.Conn, matches []vecMatch, maxResults int) ([]*index.SearchResult, error) {
	searchResults := make([]*index.SearchResult, 0)
	if len(matches) == 0 {
		return searchResults, nil
	}

	rowIDs := make([]int64, 0, len(matches))
	for _, m := range matches {
		rowIDs = append(rowIDs, m.rowID)
	}

	jsonRowIDs, err := json.Marshal(rowIDs)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	stmt, _, err := conn.Prepare(`
		SELECT m.id, m.embeddings_id, e.source, e.section_id
		FROM embeddings_vec_map m
		JOIN embeddings e ON e.id = m.embeddings_id
		WHERE m.id IN ( SELECT value FROM json_each(?) );`)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, string(jsonRowIDs)); err != nil {
		return nil, errors.WithStack(err)
	}

	type chunkInfo struct {
		embeddingsID int64
		source       string
		sectionID    string
	}

	byRowID := make(map[int64]chunkInfo, len(matches))
	for stmt.Step() {
		byRowID[stmt.ColumnInt64(0)] = chunkInfo{
			embeddingsID: stmt.ColumnInt64(1),
			source:       stmt.ColumnText(2),
			sectionID:    stmt.ColumnText(3),
		}
	}
	if err := stmt.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	// Deduplicate by chunk, keeping the best distance.
	type chunkHit struct {
		chunkInfo
		distance float64
	}

	best := map[int64]chunkHit{}
	for _, m := range matches {
		info, exists := byRowID[m.rowID]
		if !exists {
			continue
		}
		if existing, exists := best[info.embeddingsID]; !exists || m.distance < existing.distance {
			best[info.embeddingsID] = chunkHit{chunkInfo: info, distance: m.distance}
		}
	}

	hits := make([]chunkHit, 0, len(best))
	for _, h := range best {
		hits = append(hits, h)
	}
	slices.SortFunc(hits, func(a, b chunkHit) int {
		if c := cmp.Compare(a.distance, b.distance); c != 0 {
			return c
		}
		return cmp.Compare(a.embeddingsID, b.embeddingsID)
	})
	if len(hits) > maxResults {
		hits = hits[:maxResults]
	}

	mappedScores := map[string]float64{}
	mappedSections := map[string][]model.SectionID{}
	mappedSectionScores := map[string]map[model.SectionID]float64{}

	for _, h := range hits {
		if _, exists := mappedSections[h.source]; !exists {
			mappedSections[h.source] = make([]model.SectionID, 0)
			mappedSectionScores[h.source] = map[model.SectionID]float64{}
		}

		distance := h.distance
		if distance == 0 {
			distance = math.SmallestNonzeroFloat64
		}
		score := 1 / distance

		mappedSections[h.source] = append(mappedSections[h.source], model.SectionID(h.sectionID))
		mappedScores[h.source] += score
		mappedSectionScores[h.source][model.SectionID(h.sectionID)] += score
	}

	for rawSource, sectionIDs := range mappedSections {
		source, err := url.Parse(rawSource)
		if err != nil {
			return nil, errors.WithStack(err)
		}

		searchResults = append(searchResults, &index.SearchResult{
			Source:        source,
			Sections:      sectionIDs,
			Score:         mappedScores[rawSource],
			SectionScores: mappedSectionScores[rawSource],
		})
	}

	slices.SortFunc(searchResults, func(r1, r2 *index.SearchResult) int {
		return cmp.Compare(mappedScores[r2.Source.String()], mappedScores[r1.Source.String()])
	})

	return searchResults, nil
}

// acquireReader hands out a connection suitable for a read-only statement,
// together with its release function. With a read pool (NewIndexAtPath) the
// connection is one of the pooled readers — no lock is taken, WAL snapshot
// isolation guarantees a consistent view during concurrent writes. Without a
// pool (NewIndex) the single shared connection is returned under the read
// lock, preserving the historical serialization against writers.
func (i *Index) acquireReader(ctx context.Context) (*sqlite3.Conn, func(), error) {
	if i.readers != nil {
		select {
		case conn := <-i.readers:
			return conn, func() { i.readers <- conn }, nil
		case <-ctx.Done():
			return nil, nil, errors.WithStack(ctx.Err())
		}
	}

	conn, err := i.getConn(ctx)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}

	i.rwLock.RLock()
	return conn, i.rwLock.RUnlock, nil
}

func (i *Index) withRetry(ctx context.Context, fn func(ctx context.Context, conn *sqlite3.Conn) error, codes ...sqlite3.ErrorCode) error {
	conn, err := i.getConn(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	return retryOnConn(ctx, conn, fn, codes...)
}

// retryOnConn runs fn on the given connection inside a savepoint, retrying
// with exponential backoff when it fails with one of the given SQLite codes.
func retryOnConn(ctx context.Context, conn *sqlite3.Conn, fn func(ctx context.Context, conn *sqlite3.Conn) error, codes ...sqlite3.ErrorCode) error {
	backoff := 500 * time.Millisecond
	maxRetries := 10
	retries := 0

	execWithSavepoint := func() (err error) {
		save := conn.Savepoint()
		defer save.Release(&err)

		if err = fn(ctx, conn); err != nil {
			err = errors.WithStack(err)
			return
		}

		err = nil
		return
	}

	for {
		if err := execWithSavepoint(); err != nil {
			slog.DebugContext(ctx, "transaction failed", slog.Any("error", errors.WithStack(err)))

			if retries >= maxRetries {
				return errors.WithStack(err)
			}

			var sqliteErr *sqlite3.Error
			if errors.As(err, &sqliteErr) {
				if !slices.Contains(codes, sqliteErr.Code()) {
					return errors.WithStack(err)
				}

				slog.DebugContext(ctx, "will retry transaction", slog.Int("retries", retries), slog.Duration("backoff", backoff))

				retries++
				select {
				case <-ctx.Done():
					return errors.WithStack(ctx.Err())
				case <-time.After(backoff):
				}
				backoff *= 2
				continue
			}

			return errors.WithStack(err)
		}

		return nil
	}
}

// NewIndex creates a sqlite-vec backed vector index on top of the given
// connection. The connection remains owned by the caller. The embeddings model
// (WithEmbeddingsModel) and vector size (WithVectorSize) are recorded in the
// index on first use; reopening an existing index with a different model or
// size is rejected to prevent silently mixing incompatible vectors.
func NewIndex(conn *sqlite3.Conn, client llm.Client, funcs ...OptionFunc) *Index {
	opts := NewOptions(funcs...)

	if opts.CoarseQuantization && !supportsCoarse(opts.VectorSize) {
		slog.Warn("sqlitevec: coarse quantization requires a vector size divisible by 8; disabling it", slog.Int("vectorSize", opts.VectorSize))
		opts.CoarseQuantization = false
	}

	return &Index{
		maxWords:              opts.MaxWords,
		vectorSize:            opts.VectorSize,
		embeddingsConcurrency: opts.EmbeddingsConcurrency,
		coarse:                opts.CoarseQuantization,
		llm:                   client,
		model:                 opts.EmbeddingsModel,
		getConn:               createGetConn(conn, opts.EmbeddingsModel, opts.VectorSize),
	}
}

// NewIndexAtPath opens (and owns) a sqlite-vec index stored at path: one write
// connection plus a pool of read connections (WithReadPoolSize, default
// DefaultReadPoolSize), so concurrent searches run on their own connection
// under WAL snapshot isolation instead of serializing on a single one. Unlike
// NewIndex, migrations and the model/vector-size identity check run eagerly,
// and the returned index must be Closed by the caller.
func NewIndexAtPath(path string, client llm.Client, funcs ...OptionFunc) (*Index, error) {
	opts := NewOptions(funcs...)

	if opts.CoarseQuantization && !supportsCoarse(opts.VectorSize) {
		return nil, errors.Errorf("sqlitevec: coarse quantization requires a vector size divisible by 8 (got %d)", opts.VectorSize)
	}

	writer, err := sqlite3.Open(path)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	ownedConns := []*sqlite3.Conn{writer}
	closeAll := func() {
		for _, conn := range ownedConns {
			_ = conn.Close()
		}
	}

	if err := initializeConn(writer, opts.EmbeddingsModel, opts.VectorSize); err != nil {
		closeAll()
		return nil, errors.WithStack(err)
	}

	poolSize := opts.ReadPoolSize
	if poolSize <= 0 {
		poolSize = DefaultReadPoolSize
	}

	readers := make(chan *sqlite3.Conn, poolSize)
	for range poolSize {
		reader, err := sqlite3.Open(path)
		if err == nil {
			err = reader.Exec("PRAGMA busy_timeout=30000")
		}
		if err != nil {
			closeAll()
			return nil, errors.WithStack(err)
		}
		ownedConns = append(ownedConns, reader)
		readers <- reader
	}

	return &Index{
		maxWords:              opts.MaxWords,
		vectorSize:            opts.VectorSize,
		embeddingsConcurrency: opts.EmbeddingsConcurrency,
		coarse:                opts.CoarseQuantization,
		llm:                   client,
		model:                 opts.EmbeddingsModel,
		getConn: func(ctx context.Context) (*sqlite3.Conn, error) {
			return writer, nil
		},
		readers:    readers,
		ownedConns: ownedConns,
	}, nil
}

// Close closes the connections owned by the index (NewIndexAtPath). An index
// built with NewIndex owns nothing and Close is a no-op. In-flight operations
// must have completed before calling it.
func (i *Index) Close() error {
	var firstErr error
	for _, conn := range i.ownedConns {
		if err := conn.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	i.ownedConns = nil
	return errors.WithStack(firstErr)
}

var (
	_ index.Index    = &Index{}
	_ index.Semantic = &Index{}
)

// Semantic implements index.Semantic: sqlite-vec performs vector similarity
// search and therefore benefits from query expansion such as HyDE.
func (i *Index) Semantic() bool { return true }

// initializeConn applies the connection pragmas, runs the schema migrations
// (including the versioned vec0 schema upgrade) and records/verifies the
// index identity (embeddings model + vector size).
func initializeConn(conn *sqlite3.Conn, model string, vectorSize int) error {
	if err := conn.Exec("PRAGMA journal_mode=wal; PRAGMA foreign_keys=on; PRAGMA busy_timeout=30000"); err != nil {
		return errors.WithStack(err)
	}

	for _, sql := range migrations() {
		if err := conn.Exec(sql); err != nil {
			return errors.Wrapf(err, "could not execute migration '%s'", sql)
		}
	}

	if err := checkMetadata(conn, model, vectorSize); err != nil {
		return errors.WithStack(err)
	}

	if err := upgradeSchema(conn, vectorSize); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func createGetConn(conn *sqlite3.Conn, model string, vectorSize int) func(ctx context.Context) (*sqlite3.Conn, error) {
	var (
		migrateOnce sync.Once
		migrateErr  error
	)

	return func(ctx context.Context) (*sqlite3.Conn, error) {
		migrateOnce.Do(func() {
			migrateErr = initializeConn(conn, model, vectorSize)
		})
		if migrateErr != nil {
			return nil, errors.WithStack(migrateErr)
		}

		return conn, nil
	}
}

// checkMetadata records the index identity (embeddings model + vector size) on
// a fresh index, or verifies it against the configured values on an existing
// one, returning an error on mismatch.
func checkMetadata(conn *sqlite3.Conn, model string, vectorSize int) error {
	expected := map[string]string{
		"embeddings_model": model,
		"vector_size":      strconv.Itoa(vectorSize),
	}

	stored := map[string]string{}
	selectStmt, _, err := conn.Prepare("SELECT key, value FROM amoxtli_meta WHERE key IN ('embeddings_model', 'vector_size');")
	if err != nil {
		return errors.WithStack(err)
	}
	for selectStmt.Step() {
		stored[selectStmt.ColumnText(0)] = selectStmt.ColumnText(1)
	}
	if err := selectStmt.Err(); err != nil {
		selectStmt.Close()
		return errors.WithStack(err)
	}
	selectStmt.Close()

	for key, want := range expected {
		got, ok := stored[key]
		if !ok {
			if err := setMetadata(conn, key, want); err != nil {
				return errors.WithStack(err)
			}
			continue
		}
		if got != want {
			return errors.Errorf("sqlitevec: index was built with %s=%q but %q was configured; reindex or open with the original setting", key, got, want)
		}
	}

	return nil
}

func setMetadata(conn *sqlite3.Conn, key, value string) error {
	stmt, _, err := conn.Prepare("INSERT OR REPLACE INTO amoxtli_meta (key, value) VALUES (?, ?);")
	if err != nil {
		return errors.WithStack(err)
	}
	defer stmt.Close()

	if err := stmt.BindText(1, key); err != nil {
		return errors.WithStack(err)
	}
	if err := stmt.BindText(2, value); err != nil {
		return errors.WithStack(err)
	}

	return errors.WithStack(stmt.Exec())
}

func toFloat32(f64 []float64) []float32 {
	f32 := make([]float32, len(f64))
	for i, v := range f64 {
		f32[i] = float32(v)
	}
	return f32
}
