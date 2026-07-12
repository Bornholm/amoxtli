# Backend PostgreSQL

`index/postgres` combine le FTS natif (`tsvector` + `unaccent`, configuration de langue détectée automatiquement) et pgvector (KNN cosinus, index HNSW), fusionnés par Reciprocal Rank Fusion. Sans client LLM, il fonctionne en plein-texte seul. La base doit disposer des extensions `vector` et `unaccent` (images Docker [`pgvector/pgvector`](https://hub.docker.com/r/pgvector/pgvector)).

Le magasin de documents et l'index peuvent tous deux vivre dans la même base, pour un déploiement **entièrement PostgreSQL** sans aucun stockage local :

```go
dsn := "postgres://user:pass@localhost:5432/kb?sslmode=disable"

store, err := gorm.NewPostgresStore(ctx, dsn) // ingest/gorm
defer store.Close()

pool, err := pgxpool.New(ctx, dsn) // possédé par l'appelant
defer pool.Close()
pg := postgres.NewIndex(pool, llmClient) // client LLM nil = plein-texte seul

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(amoxtli.Indexer{ID: "postgres", Index: pg, Weight: 1.0}),
)
```

Voir [`example/postgres`](../example/postgres/main.go) pour un exemple complet et exécutable.
