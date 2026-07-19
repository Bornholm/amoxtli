package sqlitevec

import (
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
	getConn               func(ctx context.Context) (*sqlite3.Conn, error)
	llm                   llm.Client
	model                 string
	// rwLock allows concurrent Search operations while serializing Index/Delete
	rwLock sync.RWMutex
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

		// Delete from embeddings_collections first (has FK)
		colStmt, _, err := conn.Prepare("DELETE FROM embeddings_collections WHERE embeddings_id IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer colStmt.Close()

		jsonColIDsBytes, err := json.Marshal(idsToDelete)
		if err != nil {
			return errors.WithStack(err)
		}
		jsonColIDs := string(jsonColIDsBytes)

		if err := colStmt.BindText(1, jsonColIDs); err != nil {
			return errors.WithStack(err)
		}

		if err := colStmt.Exec(); err != nil {
			return errors.WithStack(err)
		}

		// Delete from vec0 virtual table
		vecStmt, _, err := conn.Prepare("DELETE FROM embeddings_vec WHERE rowid IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer vecStmt.Close()

		if err := vecStmt.BindText(1, jsonColIDs); err != nil {
			return errors.WithStack(err)
		}

		if err := vecStmt.Exec(); err != nil {
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

		// Delete from embeddings_collections first (has FK)
		colStmt, _, err := conn.Prepare("DELETE FROM embeddings_collections WHERE embeddings_id IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer colStmt.Close()

		jsonColIDsBytes, err := json.Marshal(idsToDelete)
		if err != nil {
			return errors.WithStack(err)
		}
		jsonColIDs := string(jsonColIDsBytes)

		if err := colStmt.BindText(1, jsonColIDs); err != nil {
			return errors.WithStack(err)
		}

		if err := colStmt.Exec(); err != nil {
			return errors.WithStack(err)
		}

		// Delete from vec0 virtual table
		vecStmt, _, err := conn.Prepare("DELETE FROM embeddings_vec WHERE rowid IN ( SELECT value FROM json_each(?) );")
		if err != nil {
			return errors.WithStack(err)
		}
		defer vecStmt.Close()

		if err := vecStmt.BindText(1, jsonColIDs); err != nil {
			return errors.WithStack(err)
		}

		if err := vecStmt.Exec(); err != nil {
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

		// Prepare vec0 statement for HNSW index
		vecStmt, _, err := conn.Prepare(fmt.Sprintf(`
			INSERT INTO embeddings_vec (rowid, embedding)
			VALUES (?, vec_normalize(vec_slice(?, 0, %d)));
		`, i.vectorSize))
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

			// Insert into vec0 for HNSW index
			if err := vecStmt.BindInt(1, embeddingsID); err != nil {
				return err
			}
			if err := vecStmt.BindBlob(2, vecBlob); err != nil {
				return err
			}
			if err := vecStmt.Exec(); err != nil {
				return err
			}
			vecStmt.Reset()

			for _, coll := range item.Section.Document().Collections() {
				if err := i.insertCollection(ctx, conn, embeddingsID, coll.ID()); err != nil {
					return errors.WithStack(err)
				}
			}
		}

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
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

	i.rwLock.RLock()
	defer i.rwLock.RUnlock()

	var searchResults []*index.SearchResult
	err = i.withRetry(ctx, func(ctx context.Context, conn *sqlite3.Conn) error {
		// Use vec0 virtual table with HNSW index for fast KNN search
		// IMPORTANT: vec0 uses <column> match <vector> syntax, not <column>.match
		// Also requires k = <number> to specify number of nearest neighbors
		sql := `
		SELECT
			e.source,
			e.section_id,
			v.distance
		FROM embeddings_vec v
		JOIN embeddings e ON v.rowid = e.id
	`

		hasCollections := len(opts.Collections) > 0
		if hasCollections {
			sql += ` JOIN embeddings_collections ec ON e.id = ec.embeddings_id`
		}

		// Use <column> match <value> syntax for vec0 KNN query
		sql += fmt.Sprintf(` WHERE v.embedding match vec_normalize(vec_slice(?, 0, %d))`, i.vectorSize)

		// Add k parameter for vec0 (number of nearest neighbors)
		sql += ` AND k = ?`

		if hasCollections {
			sql += ` AND ec.collection_id IN ( SELECT value FROM json_each(?) )`
		}

		sql += ` ORDER BY v.distance ASC`

		sql += `;`

		stmt, _, err := conn.Prepare(sql)
		if err != nil {
			return errors.WithStack(err)
		}

		defer stmt.Close()

		bindIndex := 1

		// Bind the query vector for knn_query (always first)
		if err := stmt.BindBlob(bindIndex, queryVec); err != nil {
			return errors.WithStack(err)
		}

		bindIndex++

		// Bind k parameter (number of nearest neighbors) - required for vec0
		maxResults := opts.MaxResults
		if maxResults <= 0 {
			maxResults = 10 // default
		}
		if err := stmt.BindInt(bindIndex, maxResults); err != nil {
			return errors.WithStack(err)
		}

		bindIndex++

		if hasCollections {
			jsonCollections, err := json.Marshal(opts.Collections)
			if err != nil {
				return errors.WithStack(err)
			}

			if err := stmt.BindBlob(bindIndex, jsonCollections); err != nil {
				return errors.WithStack(err)
			}
		}

		mappedScores := map[string]float64{}
		mappedSections := map[string][]model.SectionID{}
		mappedSectionScores := map[string]map[model.SectionID]float64{}

		for stmt.Step() {
			source := stmt.ColumnText(0)
			sectionID := stmt.ColumnText(1)
			distance := stmt.ColumnFloat(2)

			if _, exists := mappedSections[source]; !exists {
				mappedSections[source] = make([]model.SectionID, 0)
				mappedSectionScores[source] = map[model.SectionID]float64{}
			}

			if distance == 0 {
				distance = math.SmallestNonzeroFloat64
			}

			score := 1 / distance

			mappedSections[source] = append(mappedSections[source], model.SectionID(sectionID))
			mappedScores[source] += score
			mappedSectionScores[source][model.SectionID(sectionID)] += score
		}

		if err := stmt.Err(); err != nil {
			return errors.WithStack(err)
		}

		searchResults = make([]*index.SearchResult, 0)

		for rawSource, sectionIDs := range mappedSections {
			source, err := url.Parse(rawSource)
			if err != nil {
				return errors.WithStack(err)
			}

			searchResults = append(searchResults, &index.SearchResult{
				Source:        source,
				Sections:      sectionIDs,
				Score:         mappedScores[rawSource],
				SectionScores: mappedSectionScores[rawSource],
			})
		}

		slices.SortFunc(searchResults, func(r1 *index.SearchResult, r2 *index.SearchResult) int {
			score1 := mappedScores[r1.Source.String()]
			score2 := mappedScores[r2.Source.String()]
			if score1 > score2 {
				return -1
			}
			if score1 < score2 {
				return 1
			}
			return 0
		})

		return nil
	}, sqlite3.BUSY, sqlite3.LOCKED)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	return searchResults, nil
}

func (i *Index) withRetry(ctx context.Context, fn func(ctx context.Context, conn *sqlite3.Conn) error, codes ...sqlite3.ErrorCode) error {
	conn, err := i.getConn(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

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
	return &Index{
		maxWords:              opts.MaxWords,
		vectorSize:            opts.VectorSize,
		embeddingsConcurrency: opts.EmbeddingsConcurrency,
		llm:                   client,
		model:                 opts.EmbeddingsModel,
		getConn:               createGetConn(conn, opts.EmbeddingsModel, opts.VectorSize),
	}
}

var (
	_ index.Index    = &Index{}
	_ index.Semantic = &Index{}
)

// Semantic implements index.Semantic: sqlite-vec performs vector similarity
// search and therefore benefits from query expansion such as HyDE.
func (i *Index) Semantic() bool { return true }

func createGetConn(conn *sqlite3.Conn, model string, vectorSize int) func(ctx context.Context) (*sqlite3.Conn, error) {
	var (
		migrateOnce sync.Once
		migrateErr  error
	)

	return func(ctx context.Context) (*sqlite3.Conn, error) {
		migrateOnce.Do(func() {
			if err := conn.Exec("PRAGMA journal_mode=wal; PRAGMA foreign_keys=on; PRAGMA busy_timeout=30000"); err != nil {
				migrateErr = errors.WithStack(err)
				return
			}

			for _, sql := range migrations(vectorSize) {
				if err := conn.Exec(sql); err != nil {
					migrateErr = errors.Wrapf(err, "could not execute migration '%s'", sql)
					return
				}
			}

			if err := checkMetadata(conn, model, vectorSize); err != nil {
				migrateErr = errors.WithStack(err)
				return
			}
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
