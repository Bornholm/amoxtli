# Architecture

| Package | Rôle |
|---|---|
| `amoxtli` (racine) | Façade `Codex` : composition explicite (store + indexeurs), ingestion, recherche, backup |
| `model` | Modèle de domaine : Document, Section, Collection |
| `index` | Contrat `index.Index` + options de recherche |
| `index/bleve` | Backend plein-texte (bleve) |
| `index/sqlitevec` | Backend vectoriel (sqlite-vec + embeddings) |
| `index/postgres` | Backend hybride PostgreSQL (FTS `tsvector` + pgvector, fusion RRF) |
| `index/pipeline` | Index composite pondéré + transformers (HyDE, Judge, dédup) |
| `retrieval` | Orchestration de récupération pilotée par le grounding (γ) : vérification, re-retrieval itératif, décomposition de requête |
| `index/testsuite` | Suite de conformité pour les implémentations de `index.Index` |
| `markdown` | Parsing/chunking markdown en sections |
| `convert` | Conversion de fichiers vers markdown (`pandoc`, `libreoffice`, `genai`) |
| `task` / `task/memory` | Exécution de tâches asynchrones |
| `ingest` / `ingest/gorm` | Pipeline d'ingestion + magasin de documents (SQLite ou PostgreSQL) |
| `backup` | Abstraction snapshot/restore |

## Indexeurs personnalisés

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
