# Amoxtli

> *Amoxtli* — « livre, codex » en nahuatl.

Bibliothèque Go pour ingérer des fichiers et y faire de la recherche documentaire, avec plusieurs backends d'index au choix.

- **Recherche**: plein-texte ([bleve](https://github.com/blevesearch/bleve)), vectorielle ([sqlite-vec](https://github.com/asg017/sqlite-vec)) ou hybride PostgreSQL ([pgvector](https://github.com/pgvector/pgvector) + FTS natif). Les résultats de plusieurs index sont fusionnés par Reciprocal Rank Fusion pondérée.
- **Ingestion**: conversion de fichiers (pandoc, LibreOffice, OCR/LLM), découpage des documents markdown en sections.
- **Code source**: indexation déclaration par déclaration via [tree-sitter](https://github.com/tree-sitter/tree-sitter) en pur Go (Go, JS/TS, Python, PHP…).
- **Images**: description par LLM vision, fichiers autonomes comme images embarquées, stockage adressé par contenu et réaffichage via MCP.
- **Qualité et exploitation**: grounding (récupération vérifiée), sauvegarde et restauration des index.

## Installation

```bash
go get github.com/bornholm/amoxtli
```

> **Attention**
> 
> Le backend `index/sqlitevec` impose deux versions précises (`ncruces/go-sqlite3` v0.23.0 et `wazero` ≥ v1.9.0) à préserver côté consommateur : voir [Backend SQLite](docs/sqlite.md).

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

Exemples complets et exécutables : 
- [`example/sqlite`](example/sqlite/main.go) (SQLite + bleve, sans LLM)
- [`example/postgres`](example/postgres/main.go) (tout PostgreSQL)
- [`example/convert`](example/convert/main.go) (conversion de fichier + suivi de tâche)
- [`example/sourcecode`](example/sourcecode/main.go) (indexation de code + recherche croisée doc ↔ code) 
- [`example/vision`](example/vision/main.go) (description d'images, descripteur factice ou modèle réel).

## Ligne de commande

Le binaire [`cmd/amoxtli`](cmd/amoxtli) expose la bibliothèque sous forme d'outil : il indexe des fichiers locaux dans un espace de travail par projet (`.amoxtli/`), effectue des recherches (dont une recherche itérative `--deep` pilotée par LLM) et sert un serveur MCP (stdio ou HTTP) pour les agents.

```bash
go install github.com/bornholm/amoxtli/cmd/amoxtli@latest   # ou : make build
amoxtli init
amoxtli add ./docs/*.md                              # documentation
amoxtli add $(git ls-files '*.go')                   # code source (type=code, language=go)
amoxtli sync --base-dir . ./docs                     # arborescence, sources relatives (pas de chemin absolu indexé)
amoxtli search "modèle de concurrence"               # doc ET code
amoxtli search "modèle de concurrence" --filter '!type'        # documentation seule
amoxtli search "modèle de concurrence" --filter dirname=/docs  # métadonnées de fichier (filename, extension, size, mtime, dirname, indexed_at)
amoxtli add ./captures/*.png                         # images décrites par LLM vision (type=image)
amoxtli search "tableau de bord des ventes" --filter type=image
amoxtli image list                                   # images stockées
amoxtli image get <hash> -o schema.png               # en extraire une
amoxtli mcp stdio             # serveur MCP sur stdio (un processus par client)
amoxtli mcp http --addr :8080 # serveur MCP HTTP (processus partagé, multi-sessions)
```

Voir [docs/cli.md](docs/cli.md) pour la configuration (`config.yaml`, interpolation des secrets), les commandes CRUD et l'intégration MCP.

## Documentation

- [Ligne de commande](docs/cli.md) — CLI `amoxtli` : espace de travail, configuration, commandes CRUD, serveur MCP
- [Architecture](docs/architecture.md) — packages, indexeurs personnalisés et suite de conformité
- [Grounding (récupération vérifiée)](docs/grounding.md) — `CheckGrounding`, `SearchIterative`, décomposition, re-retrieval itératif et modes d'application (`demote` par défaut / `filter`)
- [Backend SQLite](docs/sqlite.md) — déploiement local par fichiers (sqlite-vec, build WASM embarqué, versions épinglées)
- [Backend PostgreSQL](docs/postgres.md) — déploiement entièrement PostgreSQL (FTS + pgvector, fusion RRF)
- [Convertisseurs de fichiers](docs/converters.md) — pandoc, LibreOffice, OCR/LLM
- [Indexation des images](docs/images.md) — description par LLM vision, images embarquées, coûts et cache, choix du modèle, garde-fous, réaffichage par un agent (blob store + MCP `fetch_image`)
- [Indexation de code source](docs/source-code.md) — tree-sitter pur Go, `WithSourceCode`, recherche croisée doc ↔ code, build tags
- [Tests](docs/testing.md) — tests unitaires et d'intégration (Docker, Ollama, PostgreSQL)
- [Évaluation de la pertinence](docs/evaluation.md) — Recall@k / MRR / nDCG@k, benchmarks SQuAD/BEIR, profils de récupération et résultats de référence
- [Stabilité de l'API](docs/stability.md) — politique de compatibilité (série `0.x`) et surface publique couverte
- [CHANGELOG](CHANGELOG.md) — historique des versions

L'évaluation de la pertinence (Recall@k, MRR, nDCG — avec un benchmark
multilingue sur jeux QA Hugging Face) est fournie par le package [`eval`](eval)
(voir [docs/evaluation.md](docs/evaluation.md)), et l'observabilité
(OpenTelemetry) par le package [`telemetry`](telemetry) (activée via
`amoxtli.WithObservability()`).

## Licence

[MIT](LICENSE)
