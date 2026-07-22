// Package runtime turns a workspace configuration into a live amoxtli.Codex,
// owning (and closing) the resources the library constructors require: the
// document store, the index backends and the LLM client.
package runtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/bornholm/amoxtli"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	postgresIndex "github.com/bornholm/amoxtli/index/postgres"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	"github.com/bornholm/amoxtli/ingest"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/bornholm/amoxtli/llmx"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/retrieval"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/genai/llm"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pkg/errors"
)

// Runtime is a live Codex together with the resources the CLI owns.
type Runtime struct {
	Codex *amoxtli.Codex
	Store *gormStore.Store

	lock    *Lock
	closers []io.Closer
	// llmCache is the caching decorator around the default LLM client when the
	// LLM cache is enabled; kept to report hit/miss stats on Close.
	llmCache *llmx.CachingClient
}

// Open acquires the workspace lock and wires the configuration into a Codex.
// command names the caller in the lock file for diagnostics.
func Open(ctx context.Context, ws *workspace.Workspace, cfg *config.Config, command string) (_ *Runtime, err error) {
	// rt is the runtime under construction; on any error the deferred cleanup
	// releases whatever it already holds. It must not be the named return value
	// (returning nil on error would otherwise nil it out before cleanup).
	rt := &Runtime{}

	// A client-server workspace (postgres store + index) has no exclusive
	// on-disk state, so several processes may share it; the lock is only taken
	// for file-based backends (bleve, sqlite-vec, sqlite store).
	if cfg.HasLocalState() {
		lock, err := acquireLock(ws.LockPath(), command)
		if err != nil {
			return nil, err
		}
		rt.lock = lock
	}

	defer func() {
		if err != nil {
			rt.Close()
		}
	}()

	if err := os.MkdirAll(ws.DataDir(), 0750); err != nil {
		return nil, errors.WithStack(err)
	}

	client, err := newLLMClient(ctx, ws, cfg)
	if err != nil {
		return nil, err
	}

	if cached, ok := client.(*llmx.CachingClient); ok {
		rt.llmCache = cached
	}

	// Both the gorm SQLite store and the sqlite-vec index hand a WASM build to
	// ncruces/go-sqlite3, and the first connection opened locks in the build for
	// the whole process. Force the vec0-enabled one here, before openStore opens
	// any SQLite connection — otherwise the store's vanilla build wins and vec0
	// virtual tables fail with "no such module: vec0".
	if cfg.VectorEnabled() {
		sqlitevecIndex.EnsureVecWASM()
	}

	store, err := openStore(ctx, ws, cfg)
	if err != nil {
		return nil, err
	}

	rt.Store = store

	indexers, err := rt.openIndexers(ctx, ws, cfg, client)
	if err != nil {
		return nil, err
	}

	opts := []amoxtli.Option{
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(indexers...),
	}

	stageClients, err := newStageLLMClients(ctx, ws, cfg)
	if err != nil {
		return nil, err
	}

	opts = append(opts, retrievalOptions(cfg, client, stageClients)...)

	converter, err := newFileConverter(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if converter != nil {
		opts = append(opts, amoxtli.WithFileConverter(converter))
	}

	if cfg.CodeEnabled() {
		registry := sourcecode.DefaultRegistry()

		for ext, name := range cfg.Indexing.Code.Extensions {
			lang, exists := sourcecode.ByName(name)
			if !exists {
				return nil, errors.Errorf("indexing.code.extensions: unknown language %q for extension %q", name, ext)
			}

			registry.Register(ext, lang)
		}

		opts = append(opts, amoxtli.WithSourceCode(registry))
	}

	if cfg.Indexing.MaxWordsPerSection > 0 {
		opts = append(opts, amoxtli.WithMaxWordsPerSection(cfg.Indexing.MaxWordsPerSection))
	}
	if cfg.Indexing.TaskParallelism > 0 {
		opts = append(opts, amoxtli.WithTaskParallelism(cfg.Indexing.TaskParallelism))
	}
	if cfg.Indexing.PersistentTasks {
		opts = append(opts, amoxtli.WithPersistentTasks(ws.StagingDir()))
	}

	codex, err := amoxtli.New(ctx, opts...)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	rt.Codex = codex

	return rt, nil
}

// openStore opens the document store selected by cfg.Store.Driver.
func openStore(ctx context.Context, ws *workspace.Workspace, cfg *config.Config) (*gormStore.Store, error) {
	switch cfg.Store.Driver {
	case "postgres":
		store, err := gormStore.NewPostgresStore(ctx, cfg.Store.DSN)
		if err != nil {
			return nil, errors.Wrap(err, "could not open document store")
		}
		return store, nil
	default:
		store, err := gormStore.NewSQLiteStore(ws.Resolve(cfg.Store.DSN))
		if err != nil {
			return nil, errors.Wrap(err, "could not open document store")
		}
		return store, nil
	}
}

// openIndexers builds the index backends selected by cfg.Index.Driver,
// registering their closers on rt. client may be nil (full-text-only mode).
func (rt *Runtime) openIndexers(ctx context.Context, ws *workspace.Workspace, cfg *config.Config, client llm.Client) ([]amoxtli.Indexer, error) {
	if cfg.IndexDriver() == "postgres" {
		return rt.openPostgresIndexer(ctx, cfg, client)
	}

	return rt.openLocalIndexers(ctx, ws, cfg, client)
}

// openPostgresIndexer wires the hybrid PostgreSQL index (full-text + pgvector).
// The vector leg is active only when an embeddings client is configured.
func (rt *Runtime) openPostgresIndexer(ctx context.Context, cfg *config.Config, client llm.Client) ([]amoxtli.Indexer, error) {
	pool, err := pgxpool.New(ctx, cfg.PostgresIndexDSN())
	if err != nil {
		return nil, errors.Wrap(err, "could not open postgres index pool")
	}

	rt.closers = append(rt.closers, closerFunc(func() error {
		pool.Close()
		return nil
	}))

	var vecClient llm.Client
	if cfg.HasEmbeddings() {
		vecClient = client
	}

	pgOpts := []postgresIndex.OptionFunc{}
	if cfg.HasEmbeddings() {
		pgOpts = append(pgOpts, postgresIndex.WithEmbeddingsModel(cfg.LLM.Embeddings.Model))
	}
	if cfg.Index.Postgres.VectorSize > 0 {
		pgOpts = append(pgOpts, postgresIndex.WithVectorSize(cfg.Index.Postgres.VectorSize))
	}
	if cfg.Index.Postgres.MaxWords > 0 {
		pgOpts = append(pgOpts, postgresIndex.WithMaxWords(cfg.Index.Postgres.MaxWords))
	}
	if cfg.Index.Postgres.TextSearchConfig != "" {
		pgOpts = append(pgOpts, postgresIndex.WithTextSearchConfig(cfg.Index.Postgres.TextSearchConfig))
	}

	weight := cfg.Index.Postgres.Weight
	if weight == 0 {
		weight = 1.0
	}

	return []amoxtli.Indexer{{
		ID:     "postgres",
		Index:  postgresIndex.NewIndex(pool, vecClient, pgOpts...),
		Weight: weight,
	}}, nil
}

// openLocalIndexers wires the file-based bleve full-text and sqlite-vec vector
// indexes.
func (rt *Runtime) openLocalIndexers(ctx context.Context, ws *workspace.Workspace, cfg *config.Config, client llm.Client) ([]amoxtli.Indexer, error) {
	// EnsureVecWASM is called in Open, before the store's first connection; by
	// the time we reach here the vec0-enabled build is already locked in.
	var indexers []amoxtli.Indexer

	if cfg.Index.Fulltext.Enabled {
		idx, err := bleveIndex.OpenOrCreate(ctx, ws.Resolve(cfg.Index.Fulltext.Path))
		if err != nil {
			return nil, errors.Wrap(err, "could not open full-text index")
		}

		rt.closers = append(rt.closers, idx)
		indexers = append(indexers, amoxtli.Indexer{ID: "fulltext", Index: idx, Weight: cfg.Index.Fulltext.Weight})
	}

	if cfg.VectorEnabled() {
		vecOpts := []sqlitevecIndex.OptionFunc{
			sqlitevecIndex.WithEmbeddingsModel(cfg.LLM.Embeddings.Model),
		}
		if cfg.Index.Vector.VectorSize > 0 {
			vecOpts = append(vecOpts, sqlitevecIndex.WithVectorSize(cfg.Index.Vector.VectorSize))
		}
		if cfg.Index.Vector.MaxWords > 0 {
			vecOpts = append(vecOpts, sqlitevecIndex.WithMaxWords(cfg.Index.Vector.MaxWords))
		}
		if cfg.Index.Vector.EmbeddingsConcurrency > 0 {
			vecOpts = append(vecOpts, sqlitevecIndex.WithEmbeddingsConcurrency(cfg.Index.Vector.EmbeddingsConcurrency))
		}
		if cfg.Index.Vector.ReadPool > 0 {
			vecOpts = append(vecOpts, sqlitevecIndex.WithReadPoolSize(cfg.Index.Vector.ReadPool))
		}
		if cfg.Index.Vector.CoarseQuantization {
			vecOpts = append(vecOpts, sqlitevecIndex.WithCoarseQuantization(true))
		}

		// NewIndexAtPath owns a writer plus a read pool, so concurrent searches
		// (e.g. the MCP HTTP server) don't serialize on a single connection.
		idx, err := sqlitevecIndex.NewIndexAtPath(ws.Resolve(cfg.Index.Vector.Path), client, vecOpts...)
		if err != nil {
			return nil, errors.Wrap(err, "could not open vector index")
		}

		rt.closers = append(rt.closers, idx)

		indexers = append(indexers, amoxtli.Indexer{
			ID:     "vector",
			Index:  idx,
			Weight: cfg.Index.Vector.Weight,
		})
	}

	return indexers, nil
}

// closerFunc adapts a plain func to io.Closer (e.g. pgxpool.Pool.Close, which
// returns no error).
type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// retrievalOptions maps the llm and retrieval configuration sections to Codex
// options. Without a chat client the LLM-driven pipeline stages are disabled
// (the config validation already rejected combinations requiring one). The
// retrieval profile sets the stage baseline; the explicit keys still apply on
// top of it (they can enable more stages, not disable the profile's).
func retrievalOptions(cfg *config.Config, client llm.Client, stageClients map[string]llm.Client) []amoxtli.Option {
	if !cfg.HasChat() {
		return []amoxtli.Option{
			amoxtli.WithDisableHyDE(),
			amoxtli.WithDisableJudge(),
		}
	}

	opts := []amoxtli.Option{amoxtli.WithLLMClient(client)}

	for name, stageClient := range stageClients {
		opts = append(opts, amoxtli.WithStageLLMClient(amoxtli.Stage(name), stageClient))
	}

	groundingCheck := cfg.GroundingEnabled()

	switch cfg.Retrieval.Profile {
	case config.ProfileFast:
		// No per-search chat call: search is embeddings + RRF + dedup. The
		// chat client stays available for explicitly enabled stages (--deep,
		// reranking, grounding).
		opts = append(opts, amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge())
	case config.ProfileBalanced:
		// HyDE only (one seeded, cached chat call per distinct query).
		opts = append(opts, amoxtli.WithDisableJudge())
	case config.ProfilePrecision:
		// HyDE + the fused grounding evaluator (which replaces the Judge, and
		// which GroundingEnabled already accounts for).
	}

	if cfg.Retrieval.MaxTotalWords > 0 {
		opts = append(opts, amoxtli.WithMaxTotalWords(cfg.Retrieval.MaxTotalWords))
	}

	if cfg.Retrieval.MaxSectionWords > 0 {
		opts = append(opts, amoxtli.WithMaxWordsPerSectionInPrompt(cfg.Retrieval.MaxSectionWords))
	}

	if cfg.Retrieval.Reranking {
		opts = append(opts, amoxtli.WithReranking())
	}

	if groundingCheck {
		opts = append(opts, amoxtli.WithGroundingCheck())
		if cfg.Retrieval.GroundingFailOpen {
			opts = append(opts, amoxtli.WithGroundingFailOpen())
		}
		if strings.EqualFold(cfg.Retrieval.GroundingMode, "filter") {
			opts = append(opts, amoxtli.WithGroundingMode(retrieval.GroundingFilter))
		} else if strings.EqualFold(cfg.Retrieval.GroundingMode, "demote") {
			opts = append(opts, amoxtli.WithGroundingMode(retrieval.GroundingDemote))
		}
	}

	if cfg.Retrieval.Iterative.Enabled {
		opts = append(opts, amoxtli.WithIterativeRetrieval(cfg.Retrieval.Iterative.MaxRounds))
	}

	if cfg.Retrieval.Decomposition.Enabled {
		opts = append(opts, amoxtli.WithQueryDecomposition(cfg.Retrieval.Decomposition.MaxSubQueries))
	}

	return opts
}

// Close releases everything in reverse dependency order: the Codex first
// (drains the task runner), then the indexes, the store and the lock.
func (r *Runtime) Close() error {
	if r == nil {
		return nil
	}

	var firstErr error

	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if r.Codex != nil {
		keep(r.Codex.Close())
	}

	if r.llmCache != nil {
		if hits, misses := r.llmCache.Stats(); hits+misses > 0 {
			slog.Debug("embeddings cache", slog.Int64("hits", hits), slog.Int64("misses", misses))
		}
		if hits, misses := r.llmCache.ChatStats(); hits+misses > 0 {
			slog.Debug("chat cache", slog.Int64("hits", hits), slog.Int64("misses", misses))
		}
	}

	for i := len(r.closers) - 1; i >= 0; i-- {
		keep(r.closers[i].Close())
	}

	if r.Store != nil {
		keep(r.Store.Close())
	}

	if r.lock != nil {
		keep(r.lock.Release())
	}

	return firstErr
}

// ResolveCollection resolves a collection reference (ID or label) to its ID.
// When create is true, an unknown reference is created as a new collection
// labelled ref.
func (r *Runtime) ResolveCollection(ctx context.Context, ref string, create bool) (model.CollectionID, error) {
	collections, err := r.Store.QueryCollections(ctx, ingest.QueryCollectionsOptions{HeaderOnly: true})
	if err != nil {
		return "", errors.WithStack(err)
	}

	var matches []model.CollectionID

	for _, coll := range collections {
		if string(coll.ID()) == ref {
			return coll.ID(), nil
		}
		if coll.Label() == ref {
			matches = append(matches, coll.ID())
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		if create {
			coll, err := r.Store.CreateCollection(ctx, ref)
			if err != nil {
				return "", errors.WithStack(err)
			}

			return coll.ID(), nil
		}

		return "", errors.Errorf("unknown collection %q", ref)
	default:
		return "", errors.Errorf("collection label %q is ambiguous (%d matches); use the collection ID", ref, len(matches))
	}
}

// ResolveCollections resolves a list of collection references.
func (r *Runtime) ResolveCollections(ctx context.Context, refs []string, create bool) ([]model.CollectionID, error) {
	ids := make([]model.CollectionID, 0, len(refs))

	for _, ref := range refs {
		id, err := r.ResolveCollection(ctx, ref, create)
		if err != nil {
			return nil, err
		}

		ids = append(ids, id)
	}

	return ids, nil
}
