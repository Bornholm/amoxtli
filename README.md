# Amoxtli

> *Amoxtli* — « livre, codex » en nahuatl.

Bibliothèque Go d'indexation documentaire multi-backend et d'ingestion de fichiers : recherche plein-texte ([bleve](https://github.com/blevesearch/bleve)), recherche vectorielle ([sqlite-vec](https://github.com/asg017/sqlite-vec)), recherche hybride PostgreSQL ([pgvector](https://github.com/pgvector/pgvector) + FTS natif), fusion pondérée des résultats, découpage markdown en sections, conversion de fichiers (pandoc, LibreOffice, OCR/LLM) et sauvegarde/restauration des index.

Extraite du projet [bornholm/corpus](https://github.com/Bornholm/corpus), dont elle constitue le cœur, mais indépendante de celui-ci.

**Statut : pré-v0.1.0 — API instable.**

## Installation

```bash
go get github.com/bornholm/amoxtli
```

⚠️ **Directive `replace` obligatoire** : le backend `index/sqlitevec` dépend d'un fork des bindings sqlite-vec. Les directives `replace` ne se propageant pas aux consommateurs, ajoutez à votre `go.mod` :

```
replace github.com/asg017/sqlite-vec-go-bindings => github.com/Bornholm/sqlite-vec-go-bindings v0.0.0-20250407170538-55971919e573
```

<!-- TODO: publier le fork sous son propre chemin de module pour supprimer cette contrainte. -->

## Démarrage rapide

Le magasin de documents (`WithStore`) et les indexeurs (`WithIndexers`) sont fournis explicitement, chacun construit par son propre constructeur. **L'appelant possède les ressources qu'il crée et doit les fermer** ; `codex.Close()` n'arrête que le runner de tâches.

```go
// Magasin de documents (SQLite local, ou gorm.NewPostgresStore).
store, err := gorm.NewSQLiteStore("/data/kb/data.sqlite") // ingest/gorm
if err != nil { /* ... */ }
defer store.Close()

// Index plein-texte (bleve).
bleveIdx, err := bleve.OpenOrCreate(ctx, "/data/kb/index.bleve") // index/bleve
if err != nil { /* ... */ }
defer bleveIdx.Close()

codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 1.0}),
    amoxtli.WithDisableHyDE(), amoxtli.WithDisableJudge(), // pas de client LLM
)
if err != nil { /* ... */ }
defer codex.Close()

collID, _ := codex.CreateCollection(ctx, "docs")
taskID, _ := codex.IndexFile(ctx, collID, "guide.md", file)
results, _ := codex.Search(ctx, "comment faire…", amoxtli.WithSearchMaxResults(5))
```

Exemples complets et exécutables : [`example/sqlite`](example/sqlite/main.go) (SQLite + bleve, sans LLM), [`example/postgres`](example/postgres/main.go) (tout PostgreSQL) et [`example/convert`](example/convert/main.go) (conversion de fichier + suivi de tâche).

## Architecture

| Package | Rôle |
|---|---|
| `amoxtli` (racine) | Façade `Codex` : composition explicite (store + indexeurs), ingestion, recherche, backup |
| `model` | Modèle de domaine : Document, Section, Collection |
| `index` | Contrat `index.Index` + options de recherche |
| `index/bleve` | Backend plein-texte (bleve) |
| `index/sqlitevec` | Backend vectoriel (sqlite-vec + embeddings) |
| `index/postgres` | Backend hybride PostgreSQL (FTS `tsvector` + pgvector, fusion RRF) |
| `index/pipeline` | Index composite pondéré + transformers (HyDE, Judge, dédup) |
| `index/testsuite` | Suite de conformité pour les implémentations de `index.Index` |
| `markdown` | Parsing/chunking markdown en sections |
| `convert` | Conversion de fichiers vers markdown (`pandoc`, `libreoffice`, `genai`) |
| `task` / `task/memory` | Exécution de tâches asynchrones |
| `ingest` / `ingest/gorm` | Pipeline d'ingestion + magasin de documents (SQLite ou PostgreSQL) |
| `backup` | Abstraction snapshot/restore |

### Indexeurs personnalisés

Tout type implémentant `index.Index` peut être branché dans le pipeline, avec son poids relatif dans la fusion des scores :

```go
codex, err := amoxtli.New(ctx,
    amoxtli.WithStore(store),
    amoxtli.WithIndexers(
        amoxtli.Indexer{ID: "bleve", Index: bleveIdx, Weight: 0.4},
        amoxtli.Indexer{ID: "custom", Index: myIndex, Weight: 0.6},
    ),
)
```

La conformité se vérifie avec `index/testsuite.TestIndex`.

### Backend PostgreSQL

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

Voir [`example/postgres`](example/postgres/main.go) pour un exemple complet et exécutable.

## Convertisseurs de fichiers

Binaires externes requis selon le convertisseur : `pandoc` (`convert/pandoc`), `libreoffice` (`convert/libreoffice`). `convert/genai` utilise une API d'extraction LLM/OCR (Mistral OCR, Marker).

Le convertisseur se branche via `amoxtli.WithFileConverter(...)` ; l'ingestion route alors automatiquement tout fichier non-markdown à travers lui avant parsing et indexation. `convert.NewRouted(...)` combine plusieurs convertisseurs par extension. L'ingestion étant asynchrone, `IndexFile` renvoie un `task.ID` : on suit la progression et les messages d'étape (« converting document », « parsing document », « indexing document »…) via `codex.TaskState(ctx, id)`. Voir [`example/convert`](example/convert/main.go) (implémente aussi un `convert.Converter` minimal, sans binaire externe).

## Tests

```bash
go test -short ./...                                             # sans Docker
AMOXTLI_TEST_OLLAMA=1 go test ./index/sqlitevec/ -timeout 20m    # Docker + Ollama
AMOXTLI_TEST_POSTGRES=1 go test ./index/postgres/ -timeout 10m   # Docker + PostgreSQL (FTS seul)
AMOXTLI_TEST_POSTGRES=1 go test ./ingest/gorm/ -timeout 10m      # Docker + PostgreSQL (magasin de documents)
AMOXTLI_TEST_POSTGRES=1 AMOXTLI_TEST_OLLAMA=1 \
  go test ./index/postgres/ -timeout 20m                         # Docker + PostgreSQL + Ollama (hybride)
```

## Licence

[MIT](LICENSE)
