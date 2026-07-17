# Amoxtli

> *Amoxtli* — « livre, codex » en nahuatl.

Bibliothèque Go d'indexation documentaire multi-backend et d'ingestion de fichiers : recherche plein-texte ([bleve](https://github.com/blevesearch/bleve)), recherche vectorielle ([sqlite-vec](https://github.com/asg017/sqlite-vec)), recherche hybride PostgreSQL ([pgvector](https://github.com/pgvector/pgvector) + FTS natif), fusion des résultats par Reciprocal Rank Fusion (pondérée par index), découpage markdown en sections, indexation de code source par déclaration ([tree-sitter](https://github.com/tree-sitter/tree-sitter) en pur Go : Go, JS/TS, Python, PHP…), conversion de fichiers (pandoc, LibreOffice, OCR/LLM), grounding (récupération vérifiée) et sauvegarde/restauration des index.

Extraite du projet [bornholm/corpus](https://github.com/Bornholm/corpus), dont elle constitue le cœur, mais indépendante de celui-ci.

**Statut : pré-v0.1.0 — API instable.**

## Installation

```bash
go get github.com/bornholm/amoxtli
```

Aucune directive `replace` n'est nécessaire : le backend `index/sqlitevec` embarque son propre build WASM de SQLite incluant l'extension sqlite-vec (voir `index/sqlitevec/internal/vec`).

> **Backend sqlite-vec : versions de `ncruces/go-sqlite3` et `wazero`.** Le build WASM embarqué impose deux contraintes (déclarées dans le `go.mod` d'amoxtli, à préserver côté consommateur) :
> ```
> require github.com/ncruces/go-sqlite3 v0.23.0   // ABI hôte du WASM
> require github.com/tetratelabs/wazero v1.11.0   // >= v1.9.0
> ```
> - `ncruces/go-sqlite3` **v0.23.0** : le WASM est couplé à cette ABI (`sqlite3.Binary` / `sqlite3.RuntimeConfig`, retirées dans les versions ultérieures ; les versions ≥ v0.30.5 attendent un contrat guest incompatible).
> - `tetratelabs/wazero` **≥ v1.9.0** : le compilateur de wazero v1.8.2 (version épinglée par défaut par ncruces v0.23.0) mis-compile `vec0Filter` de sqlite-vec et provoque un crash (`out of bounds memory access`) sur **toute** requête KNN. Corrigé depuis wazero v1.9.0.
>
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

Exemples complets et exécutables : [`example/sqlite`](example/sqlite/main.go) (SQLite + bleve, sans LLM), [`example/postgres`](example/postgres/main.go) (tout PostgreSQL), [`example/convert`](example/convert/main.go) (conversion de fichier + suivi de tâche) et [`example/sourcecode`](example/sourcecode/main.go) (indexation de code + recherche croisée doc ↔ code).

## Ligne de commande

Le binaire [`cmd/amoxtli`](cmd/amoxtli) expose la bibliothèque sous forme d'outil : il indexe des fichiers locaux dans un espace de travail par projet (`.amoxtli/`), effectue des recherches (dont une recherche itérative `--deep` pilotée par LLM) et sert un serveur MCP (stdio ou HTTP) pour les agents.

```bash
go install github.com/bornholm/amoxtli/cmd/amoxtli@latest   # ou : make build
amoxtli init
amoxtli add ./docs/*.md                              # documentation
amoxtli add $(git ls-files '*.go')                   # code source (type=code, language=go)
amoxtli search "modèle de concurrence"               # doc ET code
amoxtli search "modèle de concurrence" --filter "type!=code"   # documentation seule
amoxtli mcp stdio             # serveur MCP sur stdio (un processus par client)
amoxtli mcp http --addr :8080 # serveur MCP HTTP (processus partagé, multi-sessions)
```

Voir [docs/cli.md](docs/cli.md) pour la configuration (`config.yaml`, interpolation des secrets), les commandes CRUD et l'intégration MCP.

## Documentation

- [Ligne de commande](docs/cli.md) — CLI `amoxtli` : espace de travail, configuration, commandes CRUD, serveur MCP
- [Architecture](docs/architecture.md) — packages, indexeurs personnalisés et suite de conformité
- [Grounding (récupération vérifiée)](docs/grounding.md) — `CheckGrounding`, `SearchIterative`, décomposition et re-retrieval itératif
- [Backend PostgreSQL](docs/postgres.md) — déploiement entièrement PostgreSQL (FTS + pgvector, fusion RRF)
- [Convertisseurs de fichiers](docs/converters.md) — pandoc, LibreOffice, OCR/LLM
- [Indexation de code source](docs/source-code.md) — tree-sitter pur Go, `WithSourceCode`, recherche croisée doc ↔ code, build tags
- [Tests](docs/testing.md) — tests unitaires et d'intégration (Docker, Ollama, PostgreSQL)
- [Évaluation de la pertinence](docs/evaluation.md) — Recall@k / MRR / nDCG@k, benchmark multilingue sur jeux QA Hugging Face
- [Stabilité de l'API](docs/stability.md) — politique de compatibilité (série `0.x`) et surface publique couverte
- [CHANGELOG](CHANGELOG.md) — historique des versions

L'évaluation de la pertinence (Recall@k, MRR, nDCG — avec un benchmark
multilingue sur jeux QA Hugging Face) est fournie par le package [`eval`](eval)
(voir [docs/evaluation.md](docs/evaluation.md)), et l'observabilité
(OpenTelemetry) par le package [`telemetry`](telemetry) (activée via
`amoxtli.WithObservability()`).

## Licence

[MIT](LICENSE)
