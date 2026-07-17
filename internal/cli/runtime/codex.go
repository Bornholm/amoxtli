// Package runtime turns a workspace configuration into a live amoxtli.Codex,
// owning (and closing) the resources the library constructors require: the
// document store, the index backends and the LLM client.
package runtime

import (
	"context"
	"io"
	"os"

	"github.com/bornholm/amoxtli"
	bleveIndex "github.com/bornholm/amoxtli/index/bleve"
	sqlitevecIndex "github.com/bornholm/amoxtli/index/sqlitevec"
	"github.com/bornholm/amoxtli/ingest"
	gormStore "github.com/bornholm/amoxtli/ingest/gorm"
	"github.com/bornholm/amoxtli/internal/cli/config"
	"github.com/bornholm/amoxtli/internal/cli/workspace"
	"github.com/bornholm/amoxtli/model"
	"github.com/bornholm/amoxtli/sourcecode"
	"github.com/bornholm/genai/llm"
	"github.com/ncruces/go-sqlite3"
	"github.com/pkg/errors"
)

// Runtime is a live Codex together with the resources the CLI owns.
type Runtime struct {
	Codex *amoxtli.Codex
	Store *gormStore.Store

	lock    *Lock
	closers []io.Closer
}

// Open acquires the workspace lock and wires the configuration into a Codex.
// command names the caller in the lock file for diagnostics.
func Open(ctx context.Context, ws *workspace.Workspace, cfg *config.Config, command string) (rt *Runtime, err error) {
	lock, err := acquireLock(ws.LockPath(), command)
	if err != nil {
		return nil, err
	}

	rt = &Runtime{lock: lock}

	defer func() {
		if err != nil {
			rt.Close()
		}
	}()

	if err := os.MkdirAll(ws.DataDir(), 0750); err != nil {
		return nil, errors.WithStack(err)
	}

	client, err := newLLMClient(ctx, cfg)
	if err != nil {
		return nil, err
	}

	// Both the gorm store and the sqlite-vec index hand a WASM build to
	// ncruces/go-sqlite3; force the vec0-enabled one before any connection
	// opens.
	if cfg.VectorEnabled() {
		sqlitevecIndex.EnsureVecWASM()
	}

	store, err := gormStore.NewSQLiteStore(ws.Resolve(cfg.Store.DSN))
	if err != nil {
		return nil, errors.Wrap(err, "could not open document store")
	}

	rt.Store = store

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
		conn, err := sqlite3.Open(ws.Resolve(cfg.Index.Vector.Path))
		if err != nil {
			return nil, errors.Wrap(err, "could not open vector index")
		}

		rt.closers = append(rt.closers, conn)

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

		indexers = append(indexers, amoxtli.Indexer{
			ID:     "vector",
			Index:  sqlitevecIndex.NewIndex(conn, client, vecOpts...),
			Weight: cfg.Index.Vector.Weight,
		})
	}

	opts := []amoxtli.Option{
		amoxtli.WithStore(store),
		amoxtli.WithIndexers(indexers...),
	}

	opts = append(opts, retrievalOptions(cfg, client)...)

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

// retrievalOptions maps the llm and retrieval configuration sections to Codex
// options. Without a chat client the LLM-driven pipeline stages are disabled
// (the config validation already rejected combinations requiring one).
func retrievalOptions(cfg *config.Config, client llm.Client) []amoxtli.Option {
	if !cfg.HasChat() {
		return []amoxtli.Option{
			amoxtli.WithDisableHyDE(),
			amoxtli.WithDisableJudge(),
		}
	}

	opts := []amoxtli.Option{amoxtli.WithLLMClient(client)}

	if cfg.Retrieval.MaxTotalWords > 0 {
		opts = append(opts, amoxtli.WithMaxTotalWords(cfg.Retrieval.MaxTotalWords))
	}

	if cfg.Retrieval.Reranking {
		opts = append(opts, amoxtli.WithReranking())
	}

	if cfg.Retrieval.GroundingCheck || cfg.Retrieval.Iterative.Enabled {
		opts = append(opts, amoxtli.WithGroundingCheck())
		if cfg.Retrieval.GroundingFailOpen {
			opts = append(opts, amoxtli.WithGroundingFailOpen())
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
	var firstErr error

	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if r.Codex != nil {
		keep(r.Codex.Close())
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
