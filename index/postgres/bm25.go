package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/bornholm/amoxtli/index"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

// BM25 scoring of the full-text leg.
//
// ts_rank knows nothing of how rare a term is. With the 'simple' union, the
// stop words of a question are lexemes too, so the OR query matches almost
// every chunk and ranks them mostly by their stop words: on the French AN
// written questions, the full-text leg alone found the answer in the top 10
// for 63.5 % of the questions where bleve's BM25 did for 79.5 %, and took four
// times longer.
//
// The BM25 leg keeps a table of the number of chunks each lexeme occurs in,
// built with ts_stat. It drops from the query the lexemes found in more than
// bm25MaxDFRatio of the chunks, keeps the bm25MaxLexemes rarest, matches the
// chunks containing one of the rarest of those through the GIN index and
// scores them with BM25. The term frequency is the number of positions of the
// lexeme in the chunk's tsvector, and the chunk length its number of lexemes.
// Taking the term frequency as 1 lost 6 points of recall at 10 on the AN.
//
// The statistics are computed again once more than bm25RefreshRatio of the
// chunks have been written or deleted since. Every write path counts its
// chunks in amoxtli_lexeme_changes, in the database, so the writes of other
// processes and restores count too, whatever they do to the number of rows. A
// search never waits for the recomputation, except the first one on a table
// that has no statistics yet.

const (
	bm25K1 = 1.2
	bm25B  = 0.75
	// bm25MaxDFRatio drops the lexemes of the query found in more than this
	// share of the chunks: they weigh nearly nothing in BM25 and each of
	// them makes the query match a large part of the table. 0.3 gained under
	// a point of recall at 10 on the AN for twice the time; 0.03 lost 4.
	bm25MaxDFRatio = 0.1
	// bm25MinDFCutoff is the floor of that cutoff, in chunks: on a small
	// table a tenth of the chunks is less than one, which would drop every
	// lexeme, and matching all the chunks costs nothing there.
	bm25MinDFCutoff = 1000
	// bm25MaxLexemes keeps the rarest lexemes of a long query. On the AN
	// (long written questions), 24 gave a recall at 10 of .678 on val, 48
	// .712 and 96 .711; 12 fell to .627.
	bm25MaxLexemes = 48
	// bm25MatchPostings bounds the chunks the query matches: only the rarest
	// kept lexemes select the candidates, added in order while their document
	// frequencies sum to less than this number (the rarest one is always
	// added, and it may occur in up to the bm25MaxDFRatio cutoff); all the
	// kept lexemes still score them. Each lexeme under the cutoff may occur
	// in 67,000 chunks of a 672,000-chunk corpus (MLDR), and scoring every
	// match took 2.1 s per query. With 20,000 it takes 0.5 s for the same
	// recall on MLDR and the AN; 5,000 lost a point of recall at 50 on MLDR.
	bm25MatchPostings = 20000
	// bm25RefreshRatio is the share of the chunks that may change before the
	// lexeme statistics are computed again.
	bm25RefreshRatio = 0.1
	// bm25RefreshTimeout bounds a recomputation run in the background.
	bm25RefreshTimeout = 30 * time.Minute
)

func bm25Migrations() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS amoxtli_lexeme_stats (
			lexeme TEXT PRIMARY KEY,
			ndoc INTEGER NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS amoxtli_lexeme_stats_meta (
			id INTEGER PRIMARY KEY,
			chunks BIGINT NOT NULL,
			avg_len DOUBLE PRECISION NOT NULL,
			computed_at TIMESTAMPTZ NOT NULL
		);`,
		// changes: the value of amoxtli_lexeme_changes.n the statistics were
		// computed at.
		`ALTER TABLE amoxtli_lexeme_stats_meta ADD COLUMN IF NOT EXISTS changes BIGINT NOT NULL DEFAULT 0;`,
		`CREATE TABLE IF NOT EXISTS amoxtli_lexeme_changes (
			id INTEGER PRIMARY KEY,
			n BIGINT NOT NULL
		);`,
	}
}

// execer runs a statement on a pool or in a transaction.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// noteChunksChanged counts n chunks written or deleted, so that the lexeme
// statistics get computed again once enough of them changed. Run in the
// write's own transaction when there is one.
func noteChunksChanged(ctx context.Context, db execer, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := db.Exec(ctx, `INSERT INTO amoxtli_lexeme_changes (id, n) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET n = amoxtli_lexeme_changes.n + EXCLUDED.n`, n)
	return errors.WithStack(err)
}

// lexemeStatsState reads whether there are statistics, and whether more than
// bm25RefreshRatio of their chunks changed since they were computed.
func lexemeStatsState(ctx context.Context, pool *pgxpool.Pool) (have, stale bool, err error) {
	var chunks, at, now int64
	err = pool.QueryRow(ctx, `SELECT
			m.id IS NOT NULL, coalesce(m.chunks, 0), coalesce(m.changes, 0),
			coalesce((SELECT n FROM amoxtli_lexeme_changes WHERE id = 1), 0)
		FROM (SELECT 1) one
		LEFT JOIN amoxtli_lexeme_stats_meta m ON m.id = 1`).Scan(&have, &chunks, &at, &now)
	if err != nil {
		return false, false, errors.WithStack(err)
	}
	return have, have && float64(now-at) > bm25RefreshRatio*float64(max(chunks, 1)), nil
}

// ensureLexemeStats makes sure the search has statistics to read. With none
// it computes them before returning; with stale ones it starts a
// recomputation in the background and returns at once.
func (i *Index) ensureLexemeStats(ctx context.Context, pool *pgxpool.Pool) error {
	have, stale, err := lexemeStatsState(ctx, pool)
	if err != nil {
		return errors.WithStack(err)
	}
	if !have {
		return i.refreshLexemeStats(ctx, pool, true)
	}
	if stale && i.bm25Refreshing.CompareAndSwap(false, true) {
		go func() {
			defer i.bm25Refreshing.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), bm25RefreshTimeout)
			defer cancel()
			if err := i.refreshLexemeStats(ctx, pool, false); err != nil {
				slog.ErrorContext(ctx, "postgres: could not refresh the lexeme statistics", slog.Any("error", err))
			}
		}()
	}
	return nil
}

// refreshLexemeStats computes the statistics from the chunks; with
// onlyIfMissing, not when they appeared while this call waited for another
// one. Processes computing them at the same time wait on an advisory lock.
func (i *Index) refreshLexemeStats(ctx context.Context, pool *pgxpool.Pool, onlyIfMissing bool) error {
	i.bm25RefreshMu.Lock()
	defer i.bm25RefreshMu.Unlock()

	start := time.Now()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return errors.WithStack(err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('amoxtli_lexeme_stats'))`); err != nil {
		return errors.WithStack(err)
	}
	if onlyIfMissing {
		var have bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM amoxtli_lexeme_stats_meta WHERE id = 1)`).Scan(&have); err != nil {
			return errors.WithStack(err)
		}
		if have {
			return nil
		}
	}
	// The changes counted before the scan: a write landing during it counts
	// toward the next recomputation, at worst once too many.
	statements := []string{
		`DELETE FROM amoxtli_lexeme_stats`,
		`INSERT INTO amoxtli_lexeme_stats_meta (id, chunks, avg_len, computed_at, changes)
			SELECT 1, 0, 0, now(), coalesce((SELECT n FROM amoxtli_lexeme_changes WHERE id = 1), 0)
			ON CONFLICT (id) DO UPDATE SET changes = EXCLUDED.changes`,
		`INSERT INTO amoxtli_lexeme_stats (lexeme, ndoc)
			SELECT word, ndoc FROM ts_stat('SELECT tsv FROM amoxtli_chunks')`,
		`UPDATE amoxtli_lexeme_stats_meta SET (chunks, avg_len, computed_at) =
			(SELECT count(*), coalesce(avg(length(tsv)), 0), now() FROM amoxtli_chunks)
			WHERE id = 1`,
	}
	for _, sql := range statements {
		if _, err := tx.Exec(ctx, sql); err != nil {
			return errors.Wrapf(err, "could not compute the lexeme statistics")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return errors.WithStack(err)
	}
	slog.DebugContext(ctx, "postgres: lexeme statistics computed", slog.Duration("elapsed", time.Since(start)))
	return nil
}

// ftsSearchBM25 is the BM25 variant of ftsSearch.
func (i *Index) ftsSearchBM25(ctx context.Context, pool *pgxpool.Pool, query string, filter index.Filter, opts index.SearchOptions, limit int) ([]chunkHit, error) {
	if err := i.ensureLexemeStats(ctx, pool); err != nil {
		return nil, errors.WithStack(err)
	}

	config := detectTextSearchConfig(query, i.defaultConfig)

	a := &args{}
	configParam := a.bind(config)
	queryParam := a.bind(query)

	qexpr := i.queryLexemesExpr(configParam+"::regconfig", queryParam)
	sql := fmt.Sprintf(`
		WITH meta AS (
			SELECT chunks::float8 AS n, greatest(avg_len, 1) AS avg_len
			FROM amoxtli_lexeme_stats_meta WHERE id = 1
		),
		ql AS (
			SELECT DISTINCT lexeme
			FROM unnest(`+qexpr+`) AS lexeme
		),
		kept AS (
			SELECT ql.lexeme,
				ln(1 + (m.n - coalesce(s.ndoc, 0) + 0.5) / (coalesce(s.ndoc, 0) + 0.5)) AS idf
			FROM ql
			CROSS JOIN meta m
			LEFT JOIN amoxtli_lexeme_stats s ON s.lexeme = ql.lexeme
			WHERE coalesce(s.ndoc, 0) <= greatest(%[1]g * m.n, %[4]d)
			ORDER BY idf DESC
			LIMIT %[2]d
		),
		matched AS (
			SELECT r.lexeme
			FROM (
				SELECT kept.lexeme,
					sum(coalesce(s.ndoc, 0)) OVER (ORDER BY kept.idf DESC, kept.lexeme ROWS UNBOUNDED PRECEDING) - coalesce(s.ndoc, 0) AS before
				FROM kept
				LEFT JOIN amoxtli_lexeme_stats s ON s.lexeme = kept.lexeme
			) r
			WHERE r.before < %[3]d
		),
		q AS (
			SELECT (SELECT to_tsquery('simple', string_agg('''' || replace(lexeme, '''', '''''') || '''', ' | ')) FROM matched) AS query,
				array_agg(lexeme) AS lexemes
			FROM kept
		),
		cand AS (
			SELECT c.id, c.source, c.section_id, c.tsv
			FROM amoxtli_chunks c
			CROSS JOIN q`, bm25MaxDFRatio, bm25MaxLexemes, bm25MatchPostings, bm25MinDFCutoff)

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

	if len(filter) > 0 {
		clause, err := buildFilterSQL(filter, a)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		sql += ` AND (` + clause + `)`
	}

	sql += fmt.Sprintf(`
		)
		SELECT cand.id, cand.source, cand.section_id
		FROM cand
		CROSS JOIN meta m
		CROSS JOIN LATERAL (
			SELECT sum(k.idf * (tf * %[1]g) / (tf + %[2]g * (1 - %[3]g + %[3]g * length(cand.tsv) / m.avg_len))) AS score
			FROM (
				-- Only the query's lexemes are unnested: setweight marks them
				-- A and ts_filter keeps them, both in C over the tsvector.
				SELECT t.lexeme, coalesce(array_length(t.positions, 1), 1)::float8 AS tf
				FROM unnest(ts_filter(setweight(setweight(cand.tsv, 'D'), 'A', (SELECT lexemes FROM q)), '{a}')) AS t
			) terms
			JOIN kept k ON k.lexeme = terms.lexeme
		) s
		ORDER BY s.score DESC
		LIMIT %[4]d`, bm25K1+1, bm25K1, bm25B, limit)

	return queryChunkHits(ctx, pool, sql, a.values...)
}
