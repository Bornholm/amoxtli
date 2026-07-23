// Package postgres provides a hybrid index backed by PostgreSQL, combining
// native full-text search (tsvector + ts_rank) with vector similarity search
// (pgvector, cosine distance). Results from both legs are fused with
// Reciprocal Rank Fusion.
//
// The vector leg is only active when an llm.Client is provided; without one
// the index degrades gracefully to full-text search only. In both cases the
// target database must have the `vector` and `unaccent` extensions available
// (creating them requires sufficient privileges, e.g. the pgvector/pgvector
// Docker images).
package postgres

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/bornholm/amoxtli/index"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/genai/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

type Index struct {
	getPool       func(ctx context.Context) (*pgxpool.Pool, error)
	llm           llm.Client
	model         string
	maxWords      int
	vectorSize    int
	defaultConfig string
}

type Options struct {
	// EmbeddingsModel identifies the embeddings model; snapshots generated
	// with a different model are rejected on restore.
	EmbeddingsModel string
	// VectorSize is the dimension of the pgvector column. Larger embeddings
	// are truncated then re-normalized (matryoshka-style).
	VectorSize int
	// MaxWords bounds the size of the chunks sent to the embeddings model.
	MaxWords int
	// TextSearchConfig is the regconfig used when language detection is
	// inconclusive (default "simple").
	TextSearchConfig string
}

type OptionFunc func(opts *Options)

func WithEmbeddingsModel(model string) OptionFunc {
	return func(opts *Options) {
		opts.EmbeddingsModel = model
	}
}

func WithVectorSize(size int) OptionFunc {
	return func(opts *Options) {
		opts.VectorSize = size
	}
}

func WithMaxWords(maxWords int) OptionFunc {
	return func(opts *Options) {
		opts.MaxWords = maxWords
	}
}

func WithTextSearchConfig(config string) OptionFunc {
	return func(opts *Options) {
		opts.TextSearchConfig = config
	}
}

func NewOptions(funcs ...OptionFunc) *Options {
	opts := &Options{
		VectorSize:       DefaultVectorSize,
		MaxWords:         2000,
		TextSearchConfig: "simple",
	}
	for _, fn := range funcs {
		fn(opts)
	}
	return opts
}

// NewIndex creates a hybrid PostgreSQL index on top of the given pool. The
// pool remains owned by the caller. A nil llm.Client disables the vector leg
// (full-text search only).
func NewIndex(pool *pgxpool.Pool, client llm.Client, funcs ...OptionFunc) *Index {
	opts := NewOptions(funcs...)
	return &Index{
		getPool:       createGetPool(pool, migrations(opts.VectorSize)),
		llm:           client,
		model:         opts.EmbeddingsModel,
		maxWords:      opts.MaxWords,
		vectorSize:    opts.VectorSize,
		defaultConfig: opts.TextSearchConfig,
	}
}

var _ index.Index = &Index{}

type indexableChunk struct {
	Section  model.Section
	Text     string
	ChunkIdx int
}

func estimateTokens(text string) int {
	return len(text) / charsPerToken
}

const (
	charsPerToken = 4

	maxBatchItemCount = 100

	targetBatchTokens = 6000

	overlapChars = 200
)

// Index implements index.Index.
func (i *Index) Index(ctx context.Context, document model.Document, funcs ...index.OptionFunc) error {
	opts := index.NewOptions(funcs...)

	source := document.Source()
	if source == nil {
		return errors.New("source missing")
	}

	chunksToProcess, err := collectChunks(document, i.maxWords*6)
	if err != nil {
		return errors.WithStack(err)
	}

	defer func() {
		if opts.OnProgress != nil {
			opts.OnProgress(1.0)
		}
	}()

	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM amoxtli_chunks WHERE source = $1`, source.String()); err != nil {
		return errors.WithStack(err)
	}

	// Refresh this index's copy of the document metadata in the same
	// transaction as its chunks, so the two can never disagree. The delete
	// covers the case of a document re-indexed without metadata.
	if _, err := tx.Exec(ctx, `DELETE FROM amoxtli_document_metadata WHERE source = $1`, source.String()); err != nil {
		return errors.WithStack(err)
	}
	if err := writeDocumentMetadata(ctx, tx, source.String(), model.Metadata(document)); err != nil {
		return errors.WithStack(err)
	}

	totalIndexed := 0
	onChunkIndexed := func() {
		if opts.OnProgress == nil {
			return
		}

		totalIndexed++
		opts.OnProgress(float32(totalIndexed) / float32(len(chunksToProcess)))
	}

	var batchItems []*indexableChunk
	var batchTexts []string
	currentBatchTokens := 0

	flushBatch := func() error {
		if len(batchItems) == 0 {
			return nil
		}

		var embeddings [][]float64
		if i.llm != nil {
			res, err := i.llm.Embeddings(ctx, batchTexts)
			if err != nil {
				return errors.Wrap(err, "generation failed")
			}

			embeddings = res.Embeddings()

			if len(embeddings) != len(batchItems) {
				return errors.New("vector count mismatch")
			}
		}

		for idx, item := range batchItems {
			var embedding any
			if embeddings != nil {
				vec, err := normalizeVector(embeddings[idx], i.vectorSize)
				if err != nil {
					return errors.WithStack(err)
				}
				embedding = vectorLiteral(vec)
			}

			if err := i.insertChunk(ctx, tx, item, embedding); err != nil {
				return errors.WithStack(err)
			}

			onChunkIndexed()
		}

		return nil
	}

	for _, chunk := range chunksToProcess {
		tokenEst := estimateTokens(chunk.Text)

		isBatchFull := (len(batchItems) >= maxBatchItemCount) ||
			(currentBatchTokens+tokenEst >= targetBatchTokens)

		if isBatchFull {
			if err := flushBatch(); err != nil {
				return errors.WithStack(err)
			}
			batchItems = nil
			batchTexts = nil
			currentBatchTokens = 0
		}

		batchItems = append(batchItems, chunk)
		batchTexts = append(batchTexts, chunk.Text)
		currentBatchTokens += tokenEst
	}

	if err := flushBatch(); err != nil {
		return errors.WithStack(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func (i *Index) insertChunk(ctx context.Context, tx pgx.Tx, item *indexableChunk, embedding any) error {
	lang := detectTextSearchConfig(item.Text, i.defaultConfig)

	var chunkID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO amoxtli_chunks (source, section_id, chunk_index, content, lang, tsv, embedding)
		VALUES ($1, $2, $3, $4, $5,
			to_tsvector($5::text::regconfig, unaccent($4)) || to_tsvector('simple', unaccent($4)),
			$6::vector)
		RETURNING id;
	`,
		item.Section.Document().Source().String(),
		string(item.Section.ID()),
		item.ChunkIdx,
		item.Text,
		lang,
		embedding,
	).Scan(&chunkID)
	if err != nil {
		return errors.WithStack(err)
	}

	for _, coll := range item.Section.Document().Collections() {
		_, err := tx.Exec(ctx, `
			INSERT INTO amoxtli_chunk_collections (chunk_id, collection_id)
			VALUES ($1, $2)
			ON CONFLICT DO NOTHING;
		`, chunkID, string(coll.ID()))
		if err != nil {
			return errors.WithStack(err)
		}
	}

	return nil
}

func collectChunks(document model.Document, limitChars int) ([]*indexableChunk, error) {
	var chunksToProcess []*indexableChunk

	var collect func(s model.Section) error
	collect = func(s model.Section) error {
		content, err := s.Content()
		if err != nil {
			return errors.WithStack(err)
		}
		textStr := string(content)
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

// DeleteBySource implements index.Index.
func (i *Index) DeleteBySource(ctx context.Context, source *url.URL) error {
	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM amoxtli_chunks WHERE source = $1`, source.String()); err != nil {
		return errors.WithStack(err)
	}

	// A stale metadata row would let a filter match a document this index no
	// longer holds.
	if _, err := pool.Exec(ctx, `DELETE FROM amoxtli_document_metadata WHERE source = $1`, source.String()); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

// DeleteByID implements index.Index.
func (i *Index) DeleteByID(ctx context.Context, ids ...model.SectionID) error {
	pool, err := i.getPool(ctx)
	if err != nil {
		return errors.WithStack(err)
	}

	rawIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		rawIDs = append(rawIDs, string(id))
	}

	if _, err := pool.Exec(ctx, `DELETE FROM amoxtli_chunks WHERE section_id = ANY($1)`, rawIDs); err != nil {
		return errors.WithStack(err)
	}

	return nil
}

type chunkHit struct {
	ID        int64
	Source    string
	SectionID string
}

// rrfK is the standard Reciprocal Rank Fusion smoothing constant.
const rrfK = 60

// Search implements index.Index.
func (i *Index) Search(ctx context.Context, query string, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return i.search(ctx, query, nil, opts)
}

// SearchFiltered implements index.FilterableIndex: the metadata filter is
// applied inside both legs, before their fusion. Filtering after the fusion
// would let a selective filter empty a leg's top-k — the very problem the
// capability exists to solve — so each leg returns its own k matching rows.
//
// The translation is validated against the shared conformance suite
// (index/filtertest), which is what allows this index to advertise the
// capability.
func (i *Index) SearchFiltered(ctx context.Context, query string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	return i.search(ctx, query, filter, opts)
}

func (i *Index) search(ctx context.Context, query string, filter index.Filter, opts index.SearchOptions) ([]*index.SearchResult, error) {
	// Reject invalid keys before any SQL is built.
	if err := filter.Validate(); err != nil {
		return nil, errors.WithStack(err)
	}

	pool, err := i.getPool(ctx)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	limit := opts.MaxResults
	if limit <= 0 {
		limit = 10
	}

	ftsHits, err := i.ftsSearch(ctx, pool, query, filter, opts, limit)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	var vecHits []chunkHit
	if i.llm != nil {
		vecHits, err = i.vecSearch(ctx, pool, query, filter, opts, limit)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	}

	// Fuse both legs with Reciprocal Rank Fusion
	fusedScores := map[int64]float64{}
	hitsByID := map[int64]chunkHit{}

	for rank, hit := range ftsHits {
		fusedScores[hit.ID] += 1 / float64(rrfK+rank+1)
		hitsByID[hit.ID] = hit
	}
	for rank, hit := range vecHits {
		fusedScores[hit.ID] += 1 / float64(rrfK+rank+1)
		hitsByID[hit.ID] = hit
	}

	mappedScores := map[string]float64{}
	mappedSections := map[string][]model.SectionID{}
	mappedSectionScores := map[string]map[model.SectionID]float64{}

	for id, hit := range hitsByID {
		sectionID := model.SectionID(hit.SectionID)
		if _, ok := mappedSectionScores[hit.Source]; !ok {
			mappedSectionScores[hit.Source] = map[model.SectionID]float64{}
		}
		if !slices.Contains(mappedSections[hit.Source], sectionID) {
			mappedSections[hit.Source] = append(mappedSections[hit.Source], sectionID)
		}
		mappedScores[hit.Source] += fusedScores[id]
		mappedSectionScores[hit.Source][sectionID] += fusedScores[id]
	}

	searchResults := make([]*index.SearchResult, 0, len(mappedSections))

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

	return searchResults, nil
}

// ftsSearch runs the full-text leg. The query is normalized (unaccent +
// stemming for the detected language and the 'simple' config) then rewritten
// as a disjunction of lexemes, mirroring the OR semantics of a bleve match
// query; ts_rank orders the hits.
func (i *Index) ftsSearch(ctx context.Context, pool *pgxpool.Pool, query string, filter index.Filter, opts index.SearchOptions, limit int) ([]chunkHit, error) {
	config := detectTextSearchConfig(query, i.defaultConfig)

	a := &args{}
	configParam := a.bind(config)
	queryParam := a.bind(query)

	sql := `
		WITH q AS (
			SELECT to_tsquery('simple', string_agg(quoted, ' | ')) AS query
			FROM (
				SELECT '''' || replace(lexeme, '''', '''''') || '''' AS quoted
				FROM unnest(tsvector_to_array(
					to_tsvector(` + configParam + `::regconfig, unaccent(` + queryParam + `)) || to_tsvector('simple', unaccent(` + queryParam + `))
				)) AS lexeme
			) lexemes
		)
		SELECT c.id, c.source, c.section_id
		FROM amoxtli_chunks c
		CROSS JOIN q`

	if len(filter) > 0 {
		sql += metadataJoin
	}

	sql += ` WHERE q.query IS NOT NULL AND c.tsv @@ q.query`

	if len(opts.Collections) > 0 {
		sql += ` AND EXISTS (
			SELECT 1 FROM amoxtli_chunk_collections cc
			WHERE cc.chunk_id = c.id AND cc.collection_id = ANY(` + a.bind(collectionIDs(opts.Collections)) + `)
		)`
	}

	// The filter restricts the rows *before* LIMIT, so the leg returns its own
	// top-k among matching documents rather than a top-k the filter may empty.
	if len(filter) > 0 {
		clause, err := buildFilterSQL(filter, a)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		sql += ` AND (` + clause + `)`
	}

	sql += fmt.Sprintf(` ORDER BY ts_rank(c.tsv, q.query) DESC LIMIT %d`, limit)

	return queryChunkHits(ctx, pool, sql, a.values...)
}

// vecSearch runs the vector leg (cosine KNN over the pgvector column).
func (i *Index) vecSearch(ctx context.Context, pool *pgxpool.Pool, query string, filter index.Filter, opts index.SearchOptions, limit int) ([]chunkHit, error) {
	res, err := i.llm.Embeddings(ctx, []string{query})
	if err != nil {
		return nil, errors.WithStack(err)
	}

	vec, err := normalizeVector(res.Embeddings()[0], i.vectorSize)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	a := &args{}
	vecParam := a.bind(vectorLiteral(vec))

	sql := `
		SELECT c.id, c.source, c.section_id
		FROM amoxtli_chunks c`

	if len(filter) > 0 {
		sql += metadataJoin
	}

	sql += ` WHERE c.embedding IS NOT NULL`

	if len(opts.Collections) > 0 {
		sql += ` AND EXISTS (
			SELECT 1 FROM amoxtli_chunk_collections cc
			WHERE cc.chunk_id = c.id AND cc.collection_id = ANY(` + a.bind(collectionIDs(opts.Collections)) + `)
		)`
	}

	// Same reasoning as the full-text leg: filtering before LIMIT keeps the
	// KNN's k slots for matching documents.
	if len(filter) > 0 {
		clause, err := buildFilterSQL(filter, a)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		sql += ` AND (` + clause + `)`
	}

	sql += fmt.Sprintf(` ORDER BY c.embedding <=> %s::vector LIMIT %d`, vecParam, limit)

	return queryChunkHits(ctx, pool, sql, a.values...)
}

func queryChunkHits(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]chunkHit, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer rows.Close()

	var hits []chunkHit
	for rows.Next() {
		var hit chunkHit
		if err := rows.Scan(&hit.ID, &hit.Source, &hit.SectionID); err != nil {
			return nil, errors.WithStack(err)
		}
		hits = append(hits, hit)
	}

	if err := rows.Err(); err != nil {
		return nil, errors.WithStack(err)
	}

	return hits, nil
}

func collectionIDs(ids []model.CollectionID) []string {
	raw := make([]string, 0, len(ids))
	for _, id := range ids {
		raw = append(raw, string(id))
	}
	return raw
}

// normalizeVector truncates the embedding to size dimensions then
// re-normalizes it (L2), so that models emitting larger vectors than the
// column dimension remain usable (matryoshka-style truncation).
func normalizeVector(embedding []float64, size int) ([]float32, error) {
	if len(embedding) < size {
		return nil, errors.Errorf("embedding has %d dimensions, expected at least %d", len(embedding), size)
	}

	truncated := embedding[:size]

	var norm float64
	for _, v := range truncated {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		norm = 1
	}

	vec := make([]float32, size)
	for idx, v := range truncated {
		vec[idx] = float32(v / norm)
	}

	return vec, nil
}

func vectorLiteral(vec []float32) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for idx, v := range vec {
		if idx > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(strconv.FormatFloat(float64(v), 'f', -1, 32))
	}
	sb.WriteByte(']')
	return sb.String()
}
