# Architecture

| Package | Rôle |
|---|---|
| `amoxtli` (racine) | Façade `Codex` : composition explicite (store + indexeurs), ingestion, recherche, backup |
| `model` | Modèle de domaine : Document, Section, Collection |
| `index` | Contrat `index.Index` + options de recherche |
| `index/bleve` | Backend plein-texte (bleve) |
| `index/sqlitevec` | Backend vectoriel (sqlite-vec + embeddings) : vec0 partitionné par collection, quantization binaire optionnelle |
| `index/postgres` | Backend hybride PostgreSQL (FTS `tsvector` + pgvector, fusion RRF) |
| `index/pipeline` | Index composite : fusion RRF pondérée par index + transformers (HyDE, Judge, dédup) |
| `retrieval` | Orchestration de récupération pilotée par le grounding (γ) : vérification, re-retrieval itératif, décomposition de requête, reranker LLM |
| `llmx` | Décorateurs `llm.Client` : `RetryClient` (retries à backoff + rate-limit optionnel) |
| `index/testsuite` | Suite de conformité pour les implémentations de `index.Index` |
| `markdown` | Parsing/chunking markdown en sections |
| `sourcecode` | Parsing/chunking de code source en sections par déclaration (tree-sitter pur Go, métadonnées `type=code` / `language=<nom>`) |
| `convert` | Conversion de fichiers vers markdown (`pandoc`, `libreoffice`, `genai`) |
| `task` / `task/memory` / `task/gorm` | Exécution de tâches asynchrones : contrat `task.Runner`, runner en mémoire, runner persistant gorm (reprise au démarrage) |
| `ingest` / `ingest/gorm` | Pipeline d'ingestion + magasin de documents (SQLite ou PostgreSQL) |
| `backup` | Abstraction snapshot/restore |
| `eval` / `eval/hfqa` | Harnais d'évaluation de la pertinence (Recall@k / MRR / nDCG@k, segmentation par langue) + loader de jeux QA Hugging Face (format SQuAD) |
| `telemetry` | Intégration OpenTelemetry : tracer/meter partagés + instruments (latence recherche, coût LLM) |

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

### Index vectoriel sqlite-vec : partition et quantization

vec0 effectue un **scan exhaustif** (pas d'index ANN). Deux leviers en bornent
le coût :

- **Partition par collection** (schéma v2) : une ligne vec0 par
  (chunk × collection), la collection étant la *partition key* vec0. Une
  recherche filtrée ne scanne que les lignes de sa collection **et garantit k
  résultats issus de la collection** — l'ancien filtre post-KNN pouvait en
  retourner silencieusement moins (les k voisins globaux pouvaient vivre
  ailleurs). Les chunks sans collection vivent dans la partition `''` ; une
  recherche non filtrée scanne toutes les partitions et déduplique les chunks
  multi-collections. La table `embeddings_vec_map` relie les rowids vec0 aux
  chunks (`embeddings_id`) avec un index classique. La migration v1→v2 est
  automatique à l'ouverture : les blobs existants sont **re-liés, pas
  re-calculés**.
- **Quantization binaire** (`coarse_quantization`, opt-in, dimension divisible
  par 8) : recherche en deux temps — KNN Hamming sur la colonne
  `embedding_coarse bit[N]` (~30× plus rapide) présélectionnant k×8 candidats,
  puis re-scoring float et tri. Perte de qualité marginale (littérature < 1 %),
  pertinent au-delà de ~100k chunks. À valider sur le harness d'éval avant de
  l'activer en production.

### Coût LLM par recherche

Chaque étage LLM optionnel ajoute un appel réseau (latence et facturation) à
**chaque** recherche. Ordres de grandeur par configuration, hors cache :

| Configuration | Appels chat | Appels embeddings |
|---|---|---|
| Plein-texte seul (pas de `llm`) | 0 | 0 |
| + embeddings (index vectoriel) | 0 | 1 (requête) |
| + `llm.chat` (défauts : HyDE + Judge) | 2 | 1 |
| + `grounding_check` (remplace le Judge) | 2 | 1 |
| + `reranking` | 3 | 1 |
| Recherche itérative (`--deep`, décomposition + N sous-requêtes + reformulation) | 4 et plus | 1 + N |

Le prompt du Judge / reranker / évaluateur est borné par `WithMaxTotalWords`
(défaut 8000 mots, ≈ 14k tokens) et par section via
`WithMaxWordsPerSectionInPrompt` (défaut 200 mots) — c'est le poste de coût
dominant d'une recherche avec chat configuré. Trois leviers l'amortissent :

- le **cache persistant** (`llmx.CachingClient`, activé par défaut dans la
  CLI) absorbe les embeddings répétés *et* les complétions seedées (HyDE,
  Judge) : répéter une requête identique ne coûte aucun appel ;
- un **client par étage** (`WithStageLLMClient` / `llm.stages`) permet de
  pointer HyDE et le Judge vers un petit modèle rapide ;
- HyDE n'est appliqué qu'aux indexeurs sémantiques (ignoré sans eux) et
  s'exécute **en parallèle** de la branche lexicale, pas avant elle.

Sur la **qualité** apportée par ces étages, voir les résultats de référence dans
[evaluation.md](evaluation.md#résultats-de-référence-profils-de-récupération) :
l'embedder domine, HyDE apporte un gain modeste et dépendant du modèle chat, et
le grounding s'applique par défaut en mode `demote` (préserve le rappel) —
détails dans [grounding.md](grounding.md#mode-dapplication--demote-défaut--filter).

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

### Harnais d'évaluation (`eval`)

Le package `eval` mesure objectivement la qualité de récupération. Ses fonctions
de métriques sont pures et testables sans LLM : `RecallAtK` (fraction des
documents pertinents dans le top-k, ensembliste — dégénère en binaire pour une
cible unique), `ReciprocalRank` (moyenné en MRR) et `NDCGAtK` (nDCG à relevance
binaire, sensible au rang exact contrairement au recall). Un jeu golden (`query
→ sources pertinentes`) se charge depuis un JSON (`LoadDataset`) ; `Evaluate`
le déroule à travers un `Retriever` et agrège Recall@k / MRR / nDCG@k par cut-off
et par requête. Le contrat `Retriever` est découplé du `Codex` — n'importe quelle
implémentation de récupération est notable — et `FromSearchResults` adapte
`Codex.Search` (identifiant = `Source` du résultat). Les requêtes portent une
langue (`Lang`) et des tags ; `Report.ByLang()` (et le plus général
`BySegment`) ré-agrège les métriques par segment, pour l'analyse par langue ou
type de requête.

Pour l'évaluation **en conditions réelles**, le sous-package `eval/hfqa` charge
un jeu QA extractif au **format JSON SQuAD** — partagé par des datasets Hugging
Face multilingues sur de vrais documents (PIAF en français, SQuAD en anglais,
squad_es en espagnol, MLQA, XQuAD) — et le transforme en benchmark de
récupération de passages : chaque paragraphe unique devient un document, chaque
question une requête dont la source pertinente est son paragraphe d'origine. Le
test gated `TestEvaluateRealWorld` (activé par `AMOXTLI_EVAL`, piloté par
variables d'environnement) indexe le corpus et logge le rapport global puis par
langue ; il tourne en lexical pur (bleve, sans LLM) par défaut, ou en fusion
hybride si un endpoint d'embeddings est configuré. Voir
[docs/evaluation.md](evaluation.md).

### Observabilité (`telemetry`)

L'instrumentation OpenTelemetry est **toujours active mais gratuite** : sans
provider installé, les providers no-op globaux d'OTel absorbent spans et mesures.
Le package `telemetry` expose un tracer et un meter partagés sous un scope
d'instrumentation unique, plus des instruments créés paresseusement : latence de
recherche, nombre de résultats, et pour les appels LLM le nombre d'appels, la
latence et les tokens (prompt/completion). `Codex.Search` ouvre un span (longueur
de requête, nombre de résultats — jamais le texte de la requête). Le décorateur
`llmx.ObservableClient` instrumente un `llm.Client` (spans + métriques, dérivant
les tokens de complétion depuis `Usage()`) ; l'option `WithObservability()`
l'applique automatiquement au client LLM du `Codex` (HyDE, Judge, grounding,
reranker). Les embeddings émis par un index construit par l'appelant
(sqlitevec/postgres) ne sont pas couverts par cette option — l'appelant peut
envelopper lui-même son client avec `llmx.NewObservableClient`.

### Pagination par curseur

La recherche est paginée par curseur opaque. `Codex.SearchPage(...)` renvoie une page de résultats (`WithSearchMaxResults` = taille de page) et un `NextCursor` ; l'appel suivant reprend via `WithSearchCursor(cursor)`. Le curseur ancre sur la source du dernier résultat renvoyé (unique après fusion). La pagination est stable tant que l'ordre l'est (fusion RRF déterministe, ou reranking déterministe) : un contenu strictement identique produit des scores à égalité que bleve ordonne de façon non déterministe, seul cas où la pagination peut dériver. `Codex.Search(...)` reste l'entrée simple mono-page (le filtrage et le reranking s'y appliquent toujours).
