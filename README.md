# Amoxtli

> *Amoxtli* — « livre, codex » en nahuatl.

Bibliothèque Go d'indexation documentaire multi-backend et d'ingestion de fichiers : recherche plein-texte ([bleve](https://github.com/blevesearch/bleve)), recherche vectorielle ([sqlite-vec](https://github.com/asg017/sqlite-vec)), recherche hybride PostgreSQL ([pgvector](https://github.com/pgvector/pgvector) + FTS natif), fusion pondérée des résultats, découpage markdown en sections, conversion de fichiers (pandoc, LibreOffice, OCR/LLM), grounding (récupération vérifiée) et sauvegarde/restauration des index.

Extraite du projet [bornholm/corpus](https://github.com/Bornholm/corpus), dont elle constitue le cœur, mais indépendante de celui-ci.

**Statut : pré-v0.1.0 — API instable.**

## Installation

```bash
go get github.com/bornholm/amoxtli
```

Aucune directive `replace` n'est nécessaire : le backend `index/sqlitevec` embarque son propre build WASM de SQLite incluant l'extension sqlite-vec (voir `index/sqlitevec/internal/vec`).

> **Backend sqlite-vec et version de `ncruces/go-sqlite3`.** Le build WASM embarqué est couplé à l'ABI de `github.com/ncruces/go-sqlite3` **v0.23.0** (variables `sqlite3.Binary` / `sqlite3.RuntimeConfig`, retirées dans les versions ultérieures). Si vous utilisez le backend sqlite-vec, épinglez cette version dans votre `go.mod` — sinon `go get` peut sélectionner une version plus récente incompatible :
> ```
> require github.com/ncruces/go-sqlite3 v0.23.0
> ```
> Les autres backends (bleve, postgres) et le magasin SQLite (`ingest/gorm`) ne sont pas concernés.

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

## Documentation

- [Architecture](docs/architecture.md) — packages, indexeurs personnalisés et suite de conformité
- [Grounding (récupération vérifiée)](docs/grounding.md) — `CheckGrounding`, `SearchIterative`, décomposition et re-retrieval itératif
- [Backend PostgreSQL](docs/postgres.md) — déploiement entièrement PostgreSQL (FTS + pgvector, fusion RRF)
- [Convertisseurs de fichiers](docs/converters.md) — pandoc, LibreOffice, OCR/LLM
- [Tests](docs/testing.md) — tests unitaires et d'intégration (Docker, Ollama, PostgreSQL)

## Licence

[MIT](LICENSE)
