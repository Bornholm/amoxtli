# Architecture

| Package | Rôle |
|---|---|
| `amoxtli` (racine) | Façade `Codex` : composition explicite (store + indexeurs), ingestion, recherche, backup |
| `model` | Modèle de domaine : Document, Section, Collection |
| `index` | Contrat `index.Index` + options de recherche |
| `index/bleve` | Backend plein-texte (bleve) |
| `index/sqlitevec` | Backend vectoriel (sqlite-vec + embeddings) |
| `index/postgres` | Backend hybride PostgreSQL (FTS `tsvector` + pgvector, fusion RRF) |
| `index/pipeline` | Index composite : fusion RRF pondérée par index + transformers (HyDE, Judge, dédup) |
| `retrieval` | Orchestration de récupération pilotée par le grounding (γ) : vérification, re-retrieval itératif, décomposition de requête, reranker LLM |
| `llmx` | Décorateurs `llm.Client` : `RetryClient` (retries à backoff + rate-limit optionnel) |
| `index/testsuite` | Suite de conformité pour les implémentations de `index.Index` |
| `markdown` | Parsing/chunking markdown en sections |
| `convert` | Conversion de fichiers vers markdown (`pandoc`, `libreoffice`, `genai`) |
| `task` / `task/memory` / `task/gorm` | Exécution de tâches asynchrones : contrat `task.Runner`, runner en mémoire, runner persistant gorm (reprise au démarrage) |
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

### Fusion des résultats et scores

Le pipeline fusionne les listes de résultats des différents indexeurs par **Reciprocal Rank Fusion** (RRF) : chaque indexeur contribue à une source (et à ses sections) une valeur inversement proportionnelle au rang où elle apparaît, pondérée par le poids de l'indexeur. La fusion est donc sensible au rang (un résultat classé 1er pèse plus qu'un 10e) et récompense le consensus entre indexeurs. Les scores fusionnés sont exposés sur `index.SearchResult` (`Score` au niveau source, `SectionScores` par section) : un consommateur peut seuiller, ré-ordonner ou brancher un reranker.

### Transformation de requête par-index (`index.Semantic`)

La transformation de requête est appliquée **par-index** : les transformers marqués sémantiques (interface `pipeline.SemanticQueryTransformer`, implémentée par HyDE) ne sont appliqués qu'aux indexeurs déclarant l'interface de capacité `index.Semantic` (`Semantic() bool`), car l'expansion de requête aide la recherche vectorielle mais dégrade souvent le plein-texte. `index/sqlitevec` déclare cette capacité ; `index/bleve` (lexical) et `index/postgres` (hybride, gérant sa propre fusion lexical/vectoriel en interne) ne la déclarent pas et reçoivent la requête brute. Si aucun indexeur n'est sémantique, HyDE (et son appel LLM) est purement et simplement ignoré.

### Métadonnées de documents et filtrage (`index.Filter`)

Un document peut porter des métadonnées arbitraires (`map[string]any` : auteur, tags, dates, ...) via la capacité optionnelle `model.WithMetadata`, attachées à l'ingestion (`WithIndexFileMetadata`) et persistées par le store gorm (colonne JSON). À la recherche, `WithSearchFilter(...)` restreint les résultats aux documents dont les métadonnées satisfont toutes les conditions du filtre — une conjonction de `index.Condition` construites avec `index.Eq/Ne/Gt/Gte/Lt/Lte/In`. Les opérateurs ordonnés normalisent les types numériques (int/float) et reconnaissent les dates (`time.Time` / RFC 3339). Le filtrage est évalué en Go contre les métadonnées rechargées depuis le store (capacité `ingest.MetadataProvider`), donc uniformément quel que soit le backend d'index (les backends ignorent les métadonnées). Il s'applique après la fusion et avant la pagination.

### Reranking (`ingest.Reranker`)

`WithReranking()` insère un reranker LLM (`retrieval.NewLLMReranker`) dans le pipeline de recherche : il réordonne les candidats fusionnés (et filtrés) par pertinence à la requête, réutilisant le budget `WithMaxTotalWords` pour borner le prompt. Contrairement au Judge (qui filtre), le reranker ne fait que réordonner. Il s'exécute après le filtrage métadonnées et avant la pagination, donc l'ordre reranké est celui exposé et encodé dans les curseurs.

### Runner de tâches persistant (`task/gorm`)

L'ingestion (`IndexFile`, `Reindex`, `CleanupIndex`) est asynchrone : elle
planifie une tâche exécutée par un `task.Runner`. Par défaut le runner est en
mémoire (`task/memory`) — un redémarrage perd les tâches en attente. Le package
`task/gorm` fournit un runner **persistant** adossé à la même base que le store
(SQLite ou PostgreSQL), activé par `amoxtli.WithPersistentTasks(stagingDir)`.

Chaque tâche est sérialisée via son `json.Marshaler` et persistée (table
`amoxtli_tasks`) dès la planification. Au démarrage (`Run`), le runner **reprend**
le travail persisté : les tâches laissées `running` par un process interrompu
sont d'abord remises `pending` en une passe groupée *avant* le démarrage des
workers (à cet instant aucune tâche du process courant n'est encore en cours, donc
seuls de vrais orphelins sont touchés), puis toutes les `pending` sont ré-enfilées
et reconstruites via les `task.Factory` enregistrées (`RegisterFactory`, câblées
par `Manager.RegisterHandlers` pour les trois types d'ingestion). Un **claim
atomique** — transition `pending → running` gardée par `WHERE status = 'pending'`
et vérifiée sur `RowsAffected` — garantit qu'une tâche enfilée deux fois
(planifiée puis reprise, ou reprise par plusieurs runners partageant la base) ne
s'exécute qu'une seule fois. Les transitions d'état et l'état terminal sont
persistés immédiatement ; la progression intermédiaire est écrite de façon
throttlée pour ne pas marteler la base.

La reprise d'une tâche `IndexFile` suppose que son fichier mis en attente survive
au redémarrage : `WithPersistentTasks` épingle donc le répertoire de staging à un
chemin **stable** (`WithManagerStagingDir`), non supprimé à l'arrêt, au lieu du
répertoire temporaire éphémère par-process utilisé par défaut. Les handlers
d'ingestion sont idempotents (suppression par source puis ré-écriture), une tâche
reprise depuis le début est donc sûre.

### Pagination par curseur

La recherche est paginée par curseur opaque. `Codex.SearchPage(...)` renvoie une page de résultats (`WithSearchMaxResults` = taille de page) et un `NextCursor` ; l'appel suivant reprend via `WithSearchCursor(cursor)`. Le curseur ancre sur la source du dernier résultat renvoyé (unique après fusion). La pagination est stable tant que l'ordre l'est (fusion RRF déterministe, ou reranking déterministe) : un contenu strictement identique produit des scores à égalité que bleve ordonne de façon non déterministe, seul cas où la pagination peut dériver. `Codex.Search(...)` reste l'entrée simple mono-page (le filtrage et le reranking s'y appliquent toujours).
